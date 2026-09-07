package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dbarena/dbarenactl/internal/manifest"
	"github.com/dbarena/dbarenactl/internal/sweepstate"
)

// TestResolvePricingInputs_ReadsFromPricingBlock is a regression test for the
// 2026-09-01 bug: db_instance_type/disk_type/disk_baseline_iops/
// disk_baseline_throughput_mibps must be read from TestPointDef.Pricing, not
// TestPointDef.Set (Set's keys are forwarded verbatim to benchctl as --set
// flags, and none of these four are benchctl scenario inputs).
func TestResolvePricingInputs_ReadsFromPricingBlock(t *testing.T) {
	def := &manifest.TestPointDef{
		Set: map[string]string{
			"project_size":          "xlarge",
			"disk_size_gb":          "400",
			"disk_iops":             "12000",
			"disk_throughput_mibps": "500",
			"warehouses":            "640",
		},
		Pricing: map[string]string{
			"db_instance_type":               "db.m6g.xlarge",
			"disk_type":                      "gp3",
			"disk_baseline_iops":             "12000",
			"disk_baseline_throughput_mibps": "500",
		},
	}

	pi := resolvePricingInputs(def)

	if pi.instanceType != "db.m6g.xlarge" {
		t.Errorf("instanceType = %q, want %q", pi.instanceType, "db.m6g.xlarge")
	}
	if pi.diskType != "gp3" {
		t.Errorf("diskType = %q, want %q", pi.diskType, "gp3")
	}
	if pi.diskBaselineIOPS == nil || *pi.diskBaselineIOPS != 12000 {
		t.Errorf("diskBaselineIOPS = %v, want 12000", pi.diskBaselineIOPS)
	}
	if pi.diskBaselineThroughput == nil || *pi.diskBaselineThroughput != 500 {
		t.Errorf("diskBaselineThroughput = %v, want 500", pi.diskBaselineThroughput)
	}
	// disk_size_gb/disk_iops/disk_throughput_mibps/warehouses are real
	// benchctl inputs and must still come from Set, unaffected by this fix.
	if pi.diskGB == nil || *pi.diskGB != 400 {
		t.Errorf("diskGB = %v, want 400", pi.diskGB)
	}
	if pi.iops == nil || *pi.iops != 12000 {
		t.Errorf("iops = %v, want 12000", pi.iops)
	}
	if pi.throughputMbps == nil || *pi.throughputMbps != 500 {
		t.Errorf("throughputMbps = %v, want 500", pi.throughputMbps)
	}
	if pi.warehouses == nil || *pi.warehouses != 640 {
		t.Errorf("warehouses = %v, want 640", pi.warehouses)
	}
}

// TestResolvePricingInputs_FallsBackToProjectSizeWithoutPricingBlock covers
// Supabase's test points, which have no pricing: block at all (project_size
// doubles as its own compute SKU, and it has no db_instance_type or
// disk-baseline-override concept).
func TestResolvePricingInputs_FallsBackToProjectSizeWithoutPricingBlock(t *testing.T) {
	def := &manifest.TestPointDef{
		Set: map[string]string{
			"project_size": "xlarge",
			"disk_iops":    "12000",
		},
	}

	pi := resolvePricingInputs(def)

	if pi.instanceType != "xlarge" {
		t.Errorf("instanceType = %q, want %q (fallback to project_size)", pi.instanceType, "xlarge")
	}
	if pi.diskType != "" {
		t.Errorf("diskType = %q, want empty", pi.diskType)
	}
	if pi.diskBaselineIOPS != nil {
		t.Errorf("diskBaselineIOPS = %v, want nil", pi.diskBaselineIOPS)
	}
	if pi.diskBaselineThroughput != nil {
		t.Errorf("diskBaselineThroughput = %v, want nil", pi.diskBaselineThroughput)
	}
}

// TestResolvePricingInputs_PricingNeverLeaksSetOnlyKeys guards the other
// direction of the bug: a db_instance_type placed in Set (as every candidate
// file used to do) must NOT be picked up here -- it has to move to Pricing,
// or resolvePricingInputs falls back to project_size instead, exactly as it
// would for a manifest that never had the field at all.
func TestResolvePricingInputs_PricingNeverLeaksSetOnlyKeys(t *testing.T) {
	def := &manifest.TestPointDef{
		Set: map[string]string{
			"project_size":     "xlarge",
			"db_instance_type": "db.m6g.xlarge", // stale location; must be ignored
		},
	}

	pi := resolvePricingInputs(def)

	if pi.instanceType != "xlarge" {
		t.Errorf("instanceType = %q, want %q -- db_instance_type in Set must not be read", pi.instanceType, "xlarge")
	}
}

// ---- buildResultDoc + raw-clients CSV, end to end from on-disk artifacts ----

// newOrderMetricRecords builds the metricRecord set one candidate run needs
// at a given concurrency: just enough for tpmCAt/latencyFor/buildTxnMetrics
// to succeed without error (only NEW_ORDER is populated -- the other four
// TPC-C transactions are legitimately absent from some real runs too, and
// buildTxnMetrics tolerates that already).
func newOrderMetricRecords(threads string, tpm float64) []metricRecord {
	return []metricRecord{
		{FixtureThreads: threads, Name: "tpcc_tpm", Transaction: "NEW_ORDER", Status: "ok", Value: tpm},
		{FixtureThreads: threads, Name: "tpcc_count", Transaction: "NEW_ORDER", Status: "ok", Value: 1000.0},
		{FixtureThreads: threads, Name: "tpcc_duration_seconds", Transaction: "NEW_ORDER", Status: "ok", Value: 60.0},
		{FixtureThreads: threads, Name: "tpcc_latency_ms", Transaction: "NEW_ORDER", Status: "ok", Quantile: "p50", Value: 3.0},
		{FixtureThreads: threads, Name: "tpcc_latency_ms", Transaction: "NEW_ORDER", Status: "ok", Quantile: "p95", Value: 5.0},
		{FixtureThreads: threads, Name: "tpcc_latency_ms", Transaction: "NEW_ORDER", Status: "ok", Quantile: "p99", Value: 8.0},
	}
}

// newOrderRawSamplesCSV builds a two-tick benchctl raw_samples_*.csv (see
// go-tpc's --raw-samples-file) whose NEW_ORDER/ok tpm is tpm at every tick.
func newOrderRawSamplesCSV(tpm float64) string {
	return fmt.Sprintf(
		"t_seconds,transaction,status,count,tpm,avg_latency_ms,p50_latency_ms,p90_latency_ms,p95_latency_ms,p99_latency_ms,p99_9_latency_ms,max_latency_ms\n"+
			"1.0,NEW_ORDER,ok,100,%.1f,3.0,3.0,4.0,5.0,8.0,10.0,12.0\n"+
			"2.0,NEW_ORDER,ok,100,%.1f,3.1,3.1,4.1,5.1,8.1,10.1,12.1\n",
		tpm, tpm)
}

// makeCandidateRun writes a results_*.json/raw_samples_*.csv fixture pair
// into its own run directory (mirroring what benchctl fetch actually
// produces) and loads it back through loadRunMetrics -- exercising the
// real file-association path, not just hand-built in-memory structs.
func makeCandidateRun(t *testing.T, dir, runID string, iteration int, threads string, tpm float64) candidateRun {
	t.Helper()
	runDir := filepath.Join(dir, runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", runDir, err)
	}

	records := newOrderMetricRecords(threads, tpm)
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		t.Fatalf("marshal metric records: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, fmt.Sprintf("results_supabase_%d_%s.json", iteration, threads)), data, 0o644); err != nil {
		t.Fatalf("write results json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, fmt.Sprintf("raw_samples_supabase_%d_%s.csv", iteration, threads)), []byte(newOrderRawSamplesCSV(tpm)), 0o644); err != nil {
		t.Fatalf("write raw samples csv: %v", err)
	}

	metrics, rawSamples, err := loadRunMetrics(runDir)
	if err != nil {
		t.Fatalf("loadRunMetrics(%s): %v", runDir, err)
	}

	now := time.Now().UTC()
	return candidateRun{
		run: &sweepstate.Run{
			RunID:            runID,
			IterationAttempt: iteration,
			Outcome:          "success",
			LocalArtifactDir: runDir,
			CreatedAt:        now,
			UpdatedAt:        now,
		},
		metricsByThreads:    metrics,
		rawSamplesByThreads: rawSamples,
	}
}

// TestBuildResultDoc_RawClientsCSV_SelectedIterationOnly is the end-to-end
// check for the whole feature: three candidate runs at the same
// concurrency with different NEW_ORDER tpm (1000/1500/2000) go in,
// selectRepresentativeRun must pick the median (1500), and only that
// iteration should get a raw-clients-<n>.csv + a non-nil raw_metrics_file
// -- matching selectRepresentativeRun's "no pooling/intermingling across a
// test point's independent iterations" policy.
func TestBuildResultDoc_RawClientsCSV_SelectedIterationOnly(t *testing.T) {
	dir := t.TempDir()
	candidates := []candidateRun{
		makeCandidateRun(t, dir, "run-a", 1, "12", 1000.0),
		makeCandidateRun(t, dir, "run-b", 2, "12", 1500.0),
		makeCandidateRun(t, dir, "run-c", 3, "12", 2000.0),
	}

	m := &manifest.Manifest{Provider: "AWS", Workload: "tpcc"}
	tp := &sweepstate.TestPoint{Tier: "medium", BoundType: "compute", SweepID: "sweep-1"}
	def := &manifest.TestPointDef{Tier: "medium", BoundType: "compute", Set: map[string]string{"warehouses": "28"}}

	scenarioDir := filepath.Join(dir, "scenario")
	if err := os.MkdirAll(scenarioDir, 0o755); err != nil {
		t.Fatalf("mkdir scenarioDir: %v", err)
	}

	doc, err := buildResultDoc(resultDocInputs{Manifest: m, TestPoint: tp, Def: def, Successful: candidates, ManifestPath: "candidate.yaml", ScenarioDir: scenarioDir})
	if err != nil {
		t.Fatalf("buildResultDoc: %v", err)
	}

	if len(doc.Sweep) != 1 {
		t.Fatalf("want 1 sweep point (concurrency 12), got %d", len(doc.Sweep))
	}
	sp := doc.Sweep[0]
	if sp.Concurrency != 12 {
		t.Fatalf("concurrency = %d, want 12", sp.Concurrency)
	}
	if sp.Summary.Throughput.Value != 1500 {
		t.Errorf("summary throughput = %v, want 1500 (the median)", sp.Summary.Throughput.Value)
	}

	if len(sp.Iterations) != 3 {
		t.Fatalf("want 3 iterations, got %d", len(sp.Iterations))
	}
	var selectedRawFile *string
	for _, it := range sp.Iterations {
		if it.Throughput == 1500 {
			selectedRawFile = it.RawMetricsFile
			continue
		}
		if it.RawMetricsFile != nil {
			t.Errorf("non-selected iteration (throughput=%v) has raw_metrics_file = %v, want nil", it.Throughput, *it.RawMetricsFile)
		}
	}
	if selectedRawFile == nil {
		t.Fatal("selected iteration (throughput=1500) has raw_metrics_file = nil, want set")
	}
	if *selectedRawFile != "raw-clients-12.csv" {
		t.Errorf("selected iteration raw_metrics_file = %q, want %q", *selectedRawFile, "raw-clients-12.csv")
	}

	csvPath := filepath.Join(scenarioDir, "raw-clients-12.csv")
	rows := readCSVRows(t, csvPath)
	if len(rows) != 3 { // header + 2 ticks
		t.Fatalf("want 3 rows (header+2 ticks), got %d: %v", len(rows), rows)
	}
	if rows[1][1] != "1500.0" {
		t.Errorf("raw-clients-12.csv row[1].new_order_tpm = %q, want %q (run-b's data, not run-a/run-c's)", rows[1][1], "1500.0")
	}
}

// TestBuildResultDoc_RawClientsCSV_Idempotent guards against a
// second run of the same result overwriting or corrupting the CSV
// differently the second time.
func TestBuildResultDoc_RawClientsCSV_Idempotent(t *testing.T) {
	dir := t.TempDir()
	candidates := []candidateRun{
		makeCandidateRun(t, dir, "run-a", 1, "12", 1500.0),
	}
	m := &manifest.Manifest{Provider: "AWS", Workload: "tpcc"}
	tp := &sweepstate.TestPoint{Tier: "medium", BoundType: "compute", SweepID: "sweep-1"}
	def := &manifest.TestPointDef{Tier: "medium", BoundType: "compute", Set: map[string]string{"warehouses": "28"}}
	scenarioDir := filepath.Join(dir, "scenario")
	if err := os.MkdirAll(scenarioDir, 0o755); err != nil {
		t.Fatalf("mkdir scenarioDir: %v", err)
	}

	if _, err := buildResultDoc(resultDocInputs{Manifest: m, TestPoint: tp, Def: def, Successful: candidates, ManifestPath: "candidate.yaml", ScenarioDir: scenarioDir}); err != nil {
		t.Fatalf("buildResultDoc (1st): %v", err)
	}
	first, err := os.ReadFile(filepath.Join(scenarioDir, "raw-clients-12.csv"))
	if err != nil {
		t.Fatalf("read first output: %v", err)
	}
	if _, err := buildResultDoc(resultDocInputs{Manifest: m, TestPoint: tp, Def: def, Successful: candidates, ManifestPath: "candidate.yaml", ScenarioDir: scenarioDir}); err != nil {
		t.Fatalf("buildResultDoc (2nd): %v", err)
	}
	second, err := os.ReadFile(filepath.Join(scenarioDir, "raw-clients-12.csv"))
	if err != nil {
		t.Fatalf("read second output: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("output differs between runs:\n1st: %q\n2nd: %q", first, second)
	}
}

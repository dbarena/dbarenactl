package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbarena/dbarenactl/internal/manifest"
	"github.com/dbarena/dbarenactl/internal/pricing"
	"github.com/dbarena/dbarenactl/internal/sweepstate"
)

// TestResolvePricingInputs_GCPReadsDbInstanceTypeFromSet covers GCP's test
// points, which declare db_instance_type and disk_type as real benchctl
// scenario inputs (Set), same as AWS's db_instance_class -- only the
// disk-baseline fields have no benchctl-input counterpart and stay in
// TestPointDef.Pricing.
func TestResolvePricingInputs_GCPReadsDbInstanceTypeFromSet(t *testing.T) {
	def := &manifest.TestPointDef{
		Set: map[string]string{
			"db_instance_type":      "db-custom-N4-4-16384",
			"disk_type":             "HYPERDISK_BALANCED",
			"disk_size_gb":          "400",
			"disk_iops":             "12000",
			"disk_throughput_mibps": "500",
			"warehouses":            "640",
		},
		Pricing: map[string]string{
			"disk_baseline_iops":             "12000",
			"disk_baseline_throughput_mibps": "500",
		},
	}

	pi := resolvePricingInputs(manifest.ProviderGCP, def)

	if pi.instanceType != "db-custom-N4-4-16384" {
		t.Errorf("instanceType = %q, want %q", pi.instanceType, "db-custom-N4-4-16384")
	}
	if pi.diskType != "HYPERDISK_BALANCED" {
		t.Errorf("diskType = %q, want %q", pi.diskType, "HYPERDISK_BALANCED")
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

// TestResolvePricingInputs_GCPIgnoresStrayPricingKeys guards against a
// db_instance_type/disk_type key accidentally left in Pricing (the old
// location, before both became real benchctl inputs) from leaking into a
// GCP result -- GCP's switch case now only ever reads them from Set.
func TestResolvePricingInputs_GCPIgnoresStrayPricingKeys(t *testing.T) {
	def := &manifest.TestPointDef{
		Set: map[string]string{
			"db_instance_type": "db-custom-N4-2-4096",
			"disk_type":        "HYPERDISK_BALANCED",
		},
		Pricing: map[string]string{
			"db_instance_type": "should-be-ignored",
			"disk_type":        "should-be-ignored",
		},
	}

	pi := resolvePricingInputs(manifest.ProviderGCP, def)

	if pi.instanceType != "db-custom-N4-2-4096" {
		t.Errorf("instanceType = %q, want %q -- Pricing.db_instance_type must not be read", pi.instanceType, "db-custom-N4-2-4096")
	}
	if pi.diskType != "HYPERDISK_BALANCED" {
		t.Errorf("diskType = %q, want %q -- Pricing.disk_type must not be read", pi.diskType, "HYPERDISK_BALANCED")
	}
}

// TestResolvePricingInputs_AWSReadsDbInstanceClassFromSet covers AWS's test
// points, which declare db_instance_class as a real benchctl scenario input
// (Set), with disk_type/disk_baseline_* in Pricing (no benchctl-input
// counterpart). A stray Pricing.db_instance_type/Set.project_size -- neither
// of which any real AWS candidate file sets -- must not affect the result:
// AWS's switch case in resolvePricingInputs never reads either field.
func TestResolvePricingInputs_AWSReadsDbInstanceClassFromSet(t *testing.T) {
	def := &manifest.TestPointDef{
		Set: map[string]string{
			"db_instance_class": "db.m9g.xlarge",
			"disk_size_gb":      "400",
			"disk_iops":         "12000",
			"project_size":      "should-be-ignored",
		},
		Pricing: map[string]string{
			"disk_type":        "gp3",
			"db_instance_type": "should-be-ignored",
		},
	}

	pi := resolvePricingInputs(manifest.ProviderAWS, def)

	if pi.instanceType != "db.m9g.xlarge" {
		t.Errorf("instanceType = %q, want %q", pi.instanceType, "db.m9g.xlarge")
	}
	if pi.diskType != "gp3" {
		t.Errorf("diskType = %q, want %q", pi.diskType, "gp3")
	}
}

// TestResolvePricingInputs_SupabaseReadsProjectSizeFromSet covers Supabase's
// test points, which have no pricing: block at all -- project_size doubles as
// its own compute SKU, and there's no db_instance_type or
// disk-baseline-override concept.
func TestResolvePricingInputs_SupabaseReadsProjectSizeFromSet(t *testing.T) {
	def := &manifest.TestPointDef{
		Set: map[string]string{
			"project_size": "xlarge",
			"disk_iops":    "12000",
		},
	}

	pi := resolvePricingInputs(manifest.ProviderSupabase, def)

	if pi.instanceType != "xlarge" {
		t.Errorf("instanceType = %q, want %q", pi.instanceType, "xlarge")
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

// TestResolvePricingInputs_SupabaseIgnoresStraySetKeys guards against a
// db_instance_type key accidentally placed in Set (as every AWS/GCP candidate
// file used to do, historically) leaking into a Supabase result -- Supabase's
// switch case only ever reads Set["project_size"].
func TestResolvePricingInputs_SupabaseIgnoresStraySetKeys(t *testing.T) {
	def := &manifest.TestPointDef{
		Set: map[string]string{
			"project_size":     "xlarge",
			"db_instance_type": "db.m6g.xlarge", // stray; must be ignored
		},
	}

	pi := resolvePricingInputs(manifest.ProviderSupabase, def)

	if pi.instanceType != "xlarge" {
		t.Errorf("instanceType = %q, want %q -- db_instance_type in Set must not be read", pi.instanceType, "xlarge")
	}
}

// ---- computePricing / buildInstanceInfo: components + disk_type ----

// TestComputePricing_PopulatesComponentsSummingToMonthlyUSD covers the
// pricing-audit.log replacement: every pricing.CostBreakdown component must
// come through onto pricingInfo.Components (name, amount, detail intact),
// summing to exactly MonthlyUSD, and pricing.source must no longer be
// present in the marshaled JSON (dropped -- see result.schema.json).
func TestComputePricing_PopulatesComponentsSummingToMonthlyUSD(t *testing.T) {
	snapshot := &pricing.Snapshot{
		ID:        "snap-1",
		FetchedAt: time.Date(2026, 9, 11, 2, 52, 44, 0, time.UTC),
		Items: []pricing.Item{
			{SKU: "COMPUTE", Unit: "Hrs", PriceUSD: 0.032, Attributes: map[string]string{"db_instance_type": "db.t4g.small"}},
			{SKU: "STORAGE", Unit: "GB-Mo", PriceUSD: 0.115, Attributes: map[string]string{"disk_type": "gp3"}},
			{SKU: "IOPS", Unit: "IOPS-Mo", PriceUSD: 0.02, Attributes: map[string]string{"disk_type": "gp3"}},
			{SKU: "THROUGHPUT", Unit: "MBPS-Mo", PriceUSD: 0.08},
		},
	}
	diskGB, iops, throughput := 20.0, 3000.0, 125.0
	pi := pricingInputs{instanceType: "db.t4g.small", diskGB: &diskGB, iops: &iops, throughputMbps: &throughput}
	points := []sweepPointJSON{{Summary: summaryInfo{Throughput: throughputInfo{Value: 1000}}}}

	out, err := computePricing(snapshot, "aws/rds", pi, "cache-fit-small", points)
	if err != nil {
		t.Fatalf("computePricing: %v", err)
	}
	if out == nil {
		t.Fatal("computePricing returned nil pricingInfo, want non-nil")
	}

	wantNames := map[string]bool{"compute": false, "storage": false, "iops_overage": false, "throughput_overage": false}
	var gotTotal float64
	for _, c := range out.Components {
		if _, ok := wantNames[c.Name]; !ok {
			t.Errorf("unexpected component %q", c.Name)
		}
		wantNames[c.Name] = true
		gotTotal += c.AmountUSD
		if c.Detail == "" {
			t.Errorf("component %q has empty Detail", c.Name)
		}
	}
	for name, seen := range wantNames {
		if !seen {
			t.Errorf("Components missing %q", name)
		}
	}
	if math.Abs(gotTotal-out.MonthlyUSD) > 1e-9 {
		t.Errorf("sum(Components.AmountUSD) = %v, want MonthlyUSD %v", gotTotal, out.MonthlyUSD)
	}
	if points[0].Summary.TpmPerDollarMonth == nil {
		t.Error("TpmPerDollarMonth not backfilled onto sweep point")
	}

	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal pricingInfo: %v", err)
	}
	if strings.Contains(string(data), `"source"`) {
		t.Errorf("marshaled pricing JSON still contains \"source\": %s", data)
	}
	if !strings.Contains(string(data), `"components"`) {
		t.Errorf("marshaled pricing JSON missing \"components\": %s", data)
	}
}

// TestComputePricing_RoundsTpmPerDollarMonth covers the reported
// pseudoprecision bug (e.g. 3.879089433672162): tpm_per_dollar_month must
// be rounded to 2 decimal places, not raw float64 division noise.
func TestComputePricing_RoundsTpmPerDollarMonth(t *testing.T) {
	snapshot := &pricing.Snapshot{
		ID:        "snap-1",
		FetchedAt: time.Date(2026, 9, 11, 2, 52, 44, 0, time.UTC),
		Items: []pricing.Item{
			{SKU: "COMPUTE", Unit: "Hrs", PriceUSD: 0.0317, Attributes: map[string]string{"db_instance_type": "db.t4g.small"}},
			{SKU: "STORAGE", Unit: "GB-Mo", PriceUSD: 0.115, Attributes: map[string]string{"disk_type": "gp3"}},
			{SKU: "IOPS", Unit: "IOPS-Mo", PriceUSD: 0.02, Attributes: map[string]string{"disk_type": "gp3"}},
			{SKU: "THROUGHPUT", Unit: "MBPS-Mo", PriceUSD: 0.08},
		},
	}
	diskGB, iops, throughput := 20.0, 3000.0, 125.0
	pi := pricingInputs{instanceType: "db.t4g.small", diskGB: &diskGB, iops: &iops, throughputMbps: &throughput}
	points := []sweepPointJSON{{Summary: summaryInfo{Throughput: throughputInfo{Value: 1000}}}}

	out, err := computePricing(snapshot, "aws/rds", pi, "cache-fit-small", points)
	if err != nil {
		t.Fatalf("computePricing: %v", err)
	}

	got := points[0].Summary.TpmPerDollarMonth
	if got == nil {
		t.Fatal("TpmPerDollarMonth not backfilled onto sweep point")
	}
	want := pricing.RoundTo(1000/out.MonthlyUSD, 2)
	if *got != want {
		t.Errorf("TpmPerDollarMonth = %v, want %v", *got, want)
	}
	// Confirm no more than 2 decimal digits survive, not just that RoundTo
	// was applied to the right inputs.
	if rounded := pricing.RoundTo(*got, 2); rounded != *got {
		t.Errorf("TpmPerDollarMonth = %v has more than 2 decimal digits", *got)
	}
}

// TestBuildInstanceInfo_SetsDiskType covers disk_type's move from the
// (now-removed) pricing-audit.log into result.json's instance block.
func TestBuildInstanceInfo_SetsDiskType(t *testing.T) {
	diskGB := 64.0
	pi := pricingInputs{instanceType: "db.m6g.xlarge", diskType: "gp3", diskGB: &diskGB}

	info := buildInstanceInfo(nil, "aws/rds", pi, "x86_64", "PostgreSQL 17")

	if info.DiskType == nil || *info.DiskType != "gp3" {
		t.Errorf("DiskType = %v, want %q", info.DiskType, "gp3")
	}
}

// ---- buildResultDoc + raw-clients CSV, end to end from on-disk artifacts ----

// newOrderMetricRecords builds the metricRecord set one candidate run needs
// at a given concurrency: just enough for tpmAt/latencyFor/buildTxnMetrics
// to succeed without error (only NEW_ORDER is populated).
func newOrderMetricRecords(threads string, tpm float64, benchctlVersion, gotpcVersion string) []metricRecord {
	records := []metricRecord{
		{FixtureThreads: threads, Name: "tpcc_tpm", Transaction: "NEW_ORDER", Status: "ok", Value: tpm},
		{FixtureThreads: threads, Name: "tpcc_count", Transaction: "NEW_ORDER", Status: "ok", Value: 1000.0},
		{FixtureThreads: threads, Name: "tpcc_duration_seconds", Transaction: "NEW_ORDER", Status: "ok", Value: 60.0},
		{FixtureThreads: threads, Name: "tpcc_latency_ms", Transaction: "NEW_ORDER", Status: "ok", Quantile: "p50", Value: 3.0},
		{FixtureThreads: threads, Name: "tpcc_latency_ms", Transaction: "NEW_ORDER", Status: "ok", Quantile: "p95", Value: 5.0},
		{FixtureThreads: threads, Name: "tpcc_latency_ms", Transaction: "NEW_ORDER", Status: "ok", Quantile: "p99", Value: 8.0},
	}
	for i := range records {
		records[i].BenchctlVersion = benchctlVersion
		records[i].GotpcVersion = gotpcVersion
	}
	return records
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
// extra records, when given, are appended to the run's metric records -- for
// metadata rows like network_rtt that only some runs carry.
func makeCandidateRun(t *testing.T, dir, runID string, iteration int, threads string, tpm float64, benchctlVersion, gotpcVersion string, extra ...metricRecord) candidateRun {
	t.Helper()
	runDir := filepath.Join(dir, runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", runDir, err)
	}

	records := append(newOrderMetricRecords(threads, tpm, benchctlVersion, gotpcVersion), extra...)
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		t.Fatalf("marshal metric records: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, fmt.Sprintf("results_supabase_%d_%s.json", iteration, threads)), data, 0o644); err != nil {
		t.Fatalf("write results json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, fmt.Sprintf("raw_samples_supabase_%d_%s_benchmark.csv", iteration, threads)), []byte(newOrderRawSamplesCSV(tpm)), 0o644); err != nil {
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
		makeCandidateRun(t, dir, "run-a", 1, "12", 1000.0, "1.2.0+20260727-abc1234", "latest-20-geb6de81"),
		makeCandidateRun(t, dir, "run-b", 2, "12", 1500.0, "1.2.0+20260727-abc1234", "latest-20-geb6de81"),
		makeCandidateRun(t, dir, "run-c", 3, "12", 2000.0, "1.2.0+20260727-abc1234", "latest-20-geb6de81"),
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

	if got := doc.Reproducibility.BenchctlVersion; got == nil || *got != "1.2.0+20260727-abc1234" {
		t.Errorf("reproducibility.benchctl_version = %v, want %q", got, "1.2.0+20260727-abc1234")
	}
	if got := doc.Reproducibility.LoadGenerator.Version; got == nil || *got != "latest-20-geb6de81" {
		t.Errorf("reproducibility.load_generator.version = %v, want %q", got, "latest-20-geb6de81")
	}
	if got := doc.Reproducibility.DbarenactlVersion; got == nil || *got != version {
		t.Errorf("reproducibility.dbarenactl_version = %v, want %q", got, version)
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
		makeCandidateRun(t, dir, "run-a", 1, "12", 1500.0, "", ""),
	}
	m := &manifest.Manifest{Provider: "AWS", Workload: "tpcc"}
	tp := &sweepstate.TestPoint{Tier: "medium", BoundType: "compute", SweepID: "sweep-1"}
	def := &manifest.TestPointDef{Tier: "medium", BoundType: "compute", Set: map[string]string{"warehouses": "28"}}
	scenarioDir := filepath.Join(dir, "scenario")
	if err := os.MkdirAll(scenarioDir, 0o755); err != nil {
		t.Fatalf("mkdir scenarioDir: %v", err)
	}

	firstDoc, err := buildResultDoc(resultDocInputs{Manifest: m, TestPoint: tp, Def: def, Successful: candidates, ManifestPath: "candidate.yaml", ScenarioDir: scenarioDir})
	if err != nil {
		t.Fatalf("buildResultDoc (1st): %v", err)
	}
	// Records with no benchctl_version/gotpc_version (a run predating
	// benchctl's version-metadata feature) must leave these null, not "".
	if firstDoc.Reproducibility.BenchctlVersion != nil {
		t.Errorf("reproducibility.benchctl_version = %v, want nil", *firstDoc.Reproducibility.BenchctlVersion)
	}
	if firstDoc.Reproducibility.LoadGenerator.Version != nil {
		t.Errorf("reproducibility.load_generator.version = %v, want nil", *firstDoc.Reproducibility.LoadGenerator.Version)
	}
	// dbarenactl_version always comes from the running binary, regardless
	// of what the fetched metric records contain.
	if got := firstDoc.Reproducibility.DbarenactlVersion; got == nil || *got != version {
		t.Errorf("reproducibility.dbarenactl_version = %v, want %q", got, version)
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

// TestBuildResultDoc_RawClientsCSV_OnlyAtMaxConcurrency guards the sweep-wide
// policy: a raw-clients CSV (and its raw_metrics_file reference) is only
// produced at a sweep's maximum concurrency level, even though every
// concurrency level's selected iteration carries the "used for
// summary/workload_metrics" notes text.
func TestBuildResultDoc_RawClientsCSV_OnlyAtMaxConcurrency(t *testing.T) {
	dir := t.TempDir()
	makeCandidateRun(t, dir, "run-a", 1, "6", 800.0, "", "")
	candidate := makeCandidateRun(t, dir, "run-a", 1, "12", 1500.0, "", "")
	candidates := []candidateRun{candidate}

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

	if len(doc.Sweep) != 2 {
		t.Fatalf("want 2 sweep points (concurrency 6 and 12), got %d", len(doc.Sweep))
	}
	for _, sp := range doc.Sweep {
		if len(sp.Iterations) != 1 {
			t.Fatalf("concurrency %d: want 1 iteration, got %d", sp.Concurrency, len(sp.Iterations))
		}
		it := sp.Iterations[0]
		if it.Notes == nil || !strings.Contains(*it.Notes, "used for summary/workload_metrics below") {
			t.Errorf("concurrency %d: notes = %v, want the summary/workload_metrics text regardless of concurrency", sp.Concurrency, it.Notes)
		}
		switch sp.Concurrency {
		case 6:
			if it.RawMetricsFile != nil {
				t.Errorf("concurrency 6 (non-max): raw_metrics_file = %v, want nil", *it.RawMetricsFile)
			}
		case 12:
			if it.RawMetricsFile == nil || *it.RawMetricsFile != "raw-clients-12.csv" {
				t.Errorf("concurrency 12 (max): raw_metrics_file = %v, want %q", it.RawMetricsFile, "raw-clients-12.csv")
			}
		default:
			t.Fatalf("unexpected concurrency %d", sp.Concurrency)
		}
	}

	if _, err := os.Stat(filepath.Join(scenarioDir, "raw-clients-6.csv")); !os.IsNotExist(err) {
		t.Errorf("raw-clients-6.csv: stat err = %v, want IsNotExist", err)
	}
	if _, err := os.Stat(filepath.Join(scenarioDir, "raw-clients-12.csv")); err != nil {
		t.Errorf("raw-clients-12.csv: stat err = %v, want nil", err)
	}
}

// TestToolVersionInfo covers toolVersionInfo's contract: benchctl_version
// and gotpc_version are read off whichever record carries them first
// (unlike pg_version, they're stamped onto every record, not one dedicated
// metadata row), and each is independently "" when absent.
func TestToolVersionInfo(t *testing.T) {
	records := newOrderMetricRecords("12", 1500.0, "1.2.0+20260727-abc1234", "latest-20-geb6de81")
	benchctlVersion, gotpcVersion := toolVersionInfo(records)
	if benchctlVersion != "1.2.0+20260727-abc1234" {
		t.Errorf("benchctlVersion = %q, want %q", benchctlVersion, "1.2.0+20260727-abc1234")
	}
	if gotpcVersion != "latest-20-geb6de81" {
		t.Errorf("gotpcVersion = %q, want %q", gotpcVersion, "latest-20-geb6de81")
	}

	empty := newOrderMetricRecords("12", 1500.0, "", "")
	benchctlVersion, gotpcVersion = toolVersionInfo(empty)
	if benchctlVersion != "" {
		t.Errorf("benchctlVersion = %q, want empty", benchctlVersion)
	}
	if gotpcVersion != "" {
		t.Errorf("gotpcVersion = %q, want empty", gotpcVersion)
	}
}

func TestParseNetworkRTT_BenchctlFormat(t *testing.T) {
	got := parseNetworkRTT("min_us=96, median_us=154, p99_us=299, max_us=337, samples=200")
	if got == nil {
		t.Fatal("parseNetworkRTT returned nil for a well-formed value")
	}
	want := networkInfo{RTTMinUs: 96, RTTMedianUs: 154, RTTP99Us: 299, RTTMaxUs: 337, Samples: 200}
	if *got != want {
		t.Errorf("parseNetworkRTT = %+v, want %+v", *got, want)
	}
}

func TestParseNetworkRTT_RejectsIncompleteValues(t *testing.T) {
	// A half-filled block is worse than none: it reads as a measurement.
	for _, value := range []string{
		"",
		"samples=0",
		"min_us=96, median_us=154, p99_us=299, samples=200", // no max_us
		"min_us=nope, median_us=154, p99_us=299, max_us=337, samples=200",
	} {
		if got := parseNetworkRTT(value); got != nil {
			t.Errorf("parseNetworkRTT(%q) = %+v, want nil", value, *got)
		}
	}
}

func TestNetworkRTTInfo_AbsentRecordYieldsNil(t *testing.T) {
	records := newOrderMetricRecords("8", 1000, "v1", "v2")
	if got := networkRTTInfo(records); got != nil {
		t.Errorf("networkRTTInfo = %+v, want nil for records predating the probe", *got)
	}
}

func TestNetworkRTTInfo_ReadsTheMetadataRecord(t *testing.T) {
	records := append(newOrderMetricRecords("8", 1000, "v1", "v2"),
		metricRecord{FixtureThreads: "8", Name: "network_rtt",
			Value: "min_us=96, median_us=154, p99_us=299, max_us=337, samples=200"})
	got := networkRTTInfo(records)
	if got == nil {
		t.Fatal("networkRTTInfo returned nil despite a network_rtt record")
	}
	if got.RTTMedianUs != 154 {
		t.Errorf("RTTMedianUs = %d, want 154", got.RTTMedianUs)
	}
}

func TestBuildResultDoc_CarriesNetworkRTTIntoTheSweepPoint(t *testing.T) {
	dir := t.TempDir()
	rtt := metricRecord{FixtureThreads: "12", Name: "network_rtt",
		Value: "min_us=96, median_us=154, p99_us=299, max_us=337, samples=200"}
	candidates := []candidateRun{
		makeCandidateRun(t, dir, "run-a", 1, "12", 1500.0, "1.2.0", "latest", rtt),
	}
	scenarioDir := filepath.Join(dir, "scenario")
	if err := os.MkdirAll(scenarioDir, 0o755); err != nil {
		t.Fatalf("mkdir scenarioDir: %v", err)
	}

	doc, err := buildResultDoc(resultDocInputs{
		Manifest:     &manifest.Manifest{Provider: "AWS", Workload: "tpcc"},
		TestPoint:    &sweepstate.TestPoint{Tier: "medium", BoundType: "compute", SweepID: "sweep-1"},
		Def:          &manifest.TestPointDef{Tier: "medium", BoundType: "compute"},
		Successful:   candidates,
		ManifestPath: "candidate.yaml",
		ScenarioDir:  scenarioDir,
	})
	if err != nil {
		t.Fatalf("buildResultDoc: %v", err)
	}
	if len(doc.Sweep) != 1 {
		t.Fatalf("want 1 sweep point, got %d", len(doc.Sweep))
	}
	got := doc.Sweep[0].Network
	if got == nil {
		t.Fatal("sweep[0].network = nil, want the probe's figures")
	}
	want := networkInfo{RTTMinUs: 96, RTTMedianUs: 154, RTTP99Us: 299, RTTMaxUs: 337, Samples: 200}
	if *got != want {
		t.Errorf("sweep[0].network = %+v, want %+v", *got, want)
	}
}

func TestBuildResultDoc_OmitsNetworkWhenTheRunPredatesTheProbe(t *testing.T) {
	dir := t.TempDir()
	candidates := []candidateRun{makeCandidateRun(t, dir, "run-a", 1, "12", 1500.0, "1.2.0", "latest")}
	scenarioDir := filepath.Join(dir, "scenario")
	if err := os.MkdirAll(scenarioDir, 0o755); err != nil {
		t.Fatalf("mkdir scenarioDir: %v", err)
	}

	doc, err := buildResultDoc(resultDocInputs{
		Manifest:     &manifest.Manifest{Provider: "AWS", Workload: "tpcc"},
		TestPoint:    &sweepstate.TestPoint{Tier: "medium", BoundType: "compute", SweepID: "sweep-1"},
		Def:          &manifest.TestPointDef{Tier: "medium", BoundType: "compute"},
		Successful:   candidates,
		ManifestPath: "candidate.yaml",
		ScenarioDir:  scenarioDir,
	})
	if err != nil {
		t.Fatalf("buildResultDoc: %v", err)
	}
	if got := doc.Sweep[0].Network; got != nil {
		t.Errorf("sweep[0].network = %+v, want nil", *got)
	}
	// omitempty must keep the key out of the JSON entirely, not emit a null.
	blob, err := json.Marshal(doc.Sweep[0])
	if err != nil {
		t.Fatalf("marshal sweep point: %v", err)
	}
	if strings.Contains(string(blob), "\"network\"") {
		t.Errorf("sweep point JSON should omit the network key, got: %s", blob)
	}
}

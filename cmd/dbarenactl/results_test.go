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

// TestResolvePricingInputs_SupabaseReadsDiskTypeFromSet covers disk_type,
// which every Supabase manifest declares under set: (a real benchctl
// scenario input, required since scenario.yaml dropped its implicit
// default) -- not under pricing:, which no Supabase manifest ever
// populates.
func TestResolvePricingInputs_SupabaseReadsDiskTypeFromSet(t *testing.T) {
	def := &manifest.TestPointDef{
		Set:     map[string]string{"project_size": "xlarge", "disk_type": "gp3"},
		Pricing: map[string]string{"disk_type": "io2"}, // must be ignored for Supabase
	}

	pi := resolvePricingInputs(manifest.ProviderSupabase, def)

	if pi.diskType != "gp3" {
		t.Errorf("diskType = %q, want %q read from Set, not Pricing", pi.diskType, "gp3")
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
			{SKU: "COMPUTE", Unit: "Hrs", PriceUSD: 0.032, Attributes: map[string]string{"db_instance_type": "db.t4g.small", "vcpu": "2"}},
			{SKU: "STORAGE", Unit: "GB-Mo", PriceUSD: 0.115, Attributes: map[string]string{"disk_type": "gp3"}},
			{SKU: "IOPS", Unit: "IOPS-Mo", PriceUSD: 0.02, Attributes: map[string]string{"disk_type": "gp3"}},
			{SKU: "THROUGHPUT", Unit: "MBPS-Mo", PriceUSD: 0.08},
			{SKU: "CPU-CREDITS-T4G", Unit: "vCPU-Hours", PriceUSD: 0.075, Attributes: map[string]string{"instance_family": "T4G"}},
		},
	}
	diskGB, iops, throughput := 20.0, 3000.0, 125.0
	pi := pricingInputs{instanceType: "db.t4g.small", diskGB: &diskGB, iops: &iops, throughputMbps: &throughput}
	points := []sweepPointJSON{{Iterations: []iterationEntry{{WorkloadMetrics: &workloadMetrics{Transactions: map[string]txnMetrics{"NEW_ORDER": {Tpm: 1000}}}}}}}

	out, err := computePricing(snapshot, "aws/rds", pi, "cache-fit-small", points)
	if err != nil {
		t.Fatalf("computePricing: %v", err)
	}
	if out == nil {
		t.Fatal("computePricing returned nil pricingInfo, want non-nil")
	}

	wantNames := map[string]bool{"compute": false, "storage": false, "iops_overage": false, "throughput_overage": false, "cpu_credit_overage": false}
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
	if points[0].Iterations[0].TpmPerDollarMonth == nil {
		t.Error("TpmPerDollarMonth not backfilled onto iteration")
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
			{SKU: "COMPUTE", Unit: "Hrs", PriceUSD: 0.0317, Attributes: map[string]string{"db_instance_type": "db.t4g.small", "vcpu": "2"}},
			{SKU: "STORAGE", Unit: "GB-Mo", PriceUSD: 0.115, Attributes: map[string]string{"disk_type": "gp3"}},
			{SKU: "IOPS", Unit: "IOPS-Mo", PriceUSD: 0.02, Attributes: map[string]string{"disk_type": "gp3"}},
			{SKU: "THROUGHPUT", Unit: "MBPS-Mo", PriceUSD: 0.08},
			{SKU: "CPU-CREDITS-T4G", Unit: "vCPU-Hours", PriceUSD: 0.075, Attributes: map[string]string{"instance_family": "T4G"}},
		},
	}
	diskGB, iops, throughput := 20.0, 3000.0, 125.0
	pi := pricingInputs{instanceType: "db.t4g.small", diskGB: &diskGB, iops: &iops, throughputMbps: &throughput}
	points := []sweepPointJSON{{Iterations: []iterationEntry{{WorkloadMetrics: &workloadMetrics{Transactions: map[string]txnMetrics{"NEW_ORDER": {Tpm: 1000}}}}}}}

	out, err := computePricing(snapshot, "aws/rds", pi, "cache-fit-small", points)
	if err != nil {
		t.Fatalf("computePricing: %v", err)
	}

	got := points[0].Iterations[0].TpmPerDollarMonth
	if got == nil {
		t.Fatal("TpmPerDollarMonth not backfilled onto iteration")
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

	info := buildInstanceInfo(nil, "aws/rds", pi, "x86_64", "PostgreSQL 17", "", "", "")

	if info.DiskType == nil || *info.DiskType != "gp3" {
		t.Errorf("DiskType = %v, want %q", info.DiskType, "gp3")
	}
}

// TestBuildInstanceInfo_SetsPgSettingsAndOrioleDBVersion covers the two
// new instance fields sourced from benchctl's pg_settings/orioledb_version
// metadata records.
func TestBuildInstanceInfo_SetsPgSettingsAndOrioleDBVersion(t *testing.T) {
	pi := pricingInputs{instanceType: "small"}

	info := buildInstanceInfo(nil, "supabase/orioledb", pi, "aarch64", "PostgreSQL 17.11",
		"shared_buffers=12800, work_mem=5120", "OrioleDB beta 17", "")

	if info.PgSettings == nil || *info.PgSettings != "shared_buffers=12800, work_mem=5120" {
		t.Errorf("PgSettings = %v, want the raw settings string", info.PgSettings)
	}
	if info.OrioleDBVersion == nil || *info.OrioleDBVersion != "OrioleDB beta 17" {
		t.Errorf("OrioleDBVersion = %v, want %q", info.OrioleDBVersion, "OrioleDB beta 17")
	}
}

// TestBuildInstanceInfo_OmitsPgSettingsAndOrioleDBVersionWhenAbsent asserts
// that, unlike engine_version/cpu_arch, these two fields are dropped from
// the marshaled JSON (not written as null) when benchctl reported neither
// -- e.g. a non-OrioleDB engine, or a run that predates this capture.
func TestBuildInstanceInfo_OmitsPgSettingsAndOrioleDBVersionWhenAbsent(t *testing.T) {
	pi := pricingInputs{instanceType: "db.m6g.xlarge"}

	info := buildInstanceInfo(nil, "aws/rds", pi, "x86_64", "PostgreSQL 17", "", "", "")

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal instanceInfo: %v", err)
	}
	if strings.Contains(string(data), "pg_settings") {
		t.Errorf("expected no pg_settings key in %s", data)
	}
	if strings.Contains(string(data), "orioledb_version") {
		t.Errorf("expected no orioledb_version key in %s", data)
	}
}

// ---- buildResultDoc + raw-clients CSV, end to end from on-disk artifacts ----

// newOrderMetricRecords builds the metricRecord set one candidate run needs
// at a given concurrency: just enough for tpmAt/buildTxnMetrics to succeed
// without error (only NEW_ORDER is populated).
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
	stepWindows, err := loadStepWindows(runDir)
	if err != nil {
		t.Fatalf("loadStepWindows(%s): %v", runDir, err)
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
		stepWindows:         stepWindows,
	}
}

// buildTestDoc runs buildResultDoc for candidates with a minimal manifest and
// test point, writing CSVs into scenarioDir.
func buildTestDoc(t *testing.T, candidates []candidateRun, scenarioDir string) *resultDoc {
	t.Helper()
	if err := os.MkdirAll(scenarioDir, 0o755); err != nil {
		t.Fatalf("mkdir scenarioDir: %v", err)
	}
	doc, err := buildResultDoc(resultDocInputs{
		Manifest:     &manifest.Manifest{Provider: "AWS", Workload: "tpcc"},
		TestPoint:    &sweepstate.TestPoint{Tier: "medium", BoundType: "compute", SweepID: "sweep-1"},
		Def:          &manifest.TestPointDef{Tier: "medium", BoundType: "compute", Set: map[string]string{"warehouses": "28"}},
		Successful:   candidates,
		ManifestPath: "candidate.yaml",
		ScenarioDir:  scenarioDir,
	})
	if err != nil {
		t.Fatalf("buildResultDoc: %v", err)
	}
	return doc
}

// newOrderTpm returns an iteration's NEW_ORDER tpm.
func newOrderTpm(it iterationEntry) float64 {
	return it.WorkloadMetrics.Transactions["NEW_ORDER"].Tpm
}

// TestBuildResultDoc_MarksPeakAndMedianAndWritesEveryCSV is the end-to-end
// check: three runs at two concurrency levels go in. Every level marks its
// median run, the level with the highest median is the peak, and every run
// at every level gets its own raw-clients CSV.
func TestBuildResultDoc_MarksPeakAndMedianAndWritesEveryCSV(t *testing.T) {
	dir := t.TempDir()
	tpm := map[string]map[string]float64{
		"run-a": {"6": 900, "12": 1000},
		"run-b": {"6": 800, "12": 1500},
		"run-c": {"6": 700, "12": 2000},
	}
	var candidates []candidateRun
	for i, runID := range []string{"run-a", "run-b", "run-c"} {
		makeCandidateRun(t, dir, runID, i+1, "6", tpm[runID]["6"], "1.2.0+20260727-abc1234", "latest-20-geb6de81")
		candidates = append(candidates, makeCandidateRun(t, dir, runID, i+1, "12", tpm[runID]["12"], "1.2.0+20260727-abc1234", "latest-20-geb6de81"))
	}
	scenarioDir := filepath.Join(dir, "scenario")
	doc := buildTestDoc(t, candidates, scenarioDir)

	if doc.SchemaVersion != "2.0.0" {
		t.Errorf("schema_version = %q, want 2.0.0", doc.SchemaVersion)
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

	if len(doc.Sweep) != 2 {
		t.Fatalf("want 2 sweep points (concurrency 6 and 12), got %d", len(doc.Sweep))
	}
	wantMedian := map[int]float64{6: 800, 12: 1500}
	for _, sp := range doc.Sweep {
		if want := sp.Concurrency == 12; sp.Peak != want {
			t.Errorf("concurrency %d: peak = %v, want %v", sp.Concurrency, sp.Peak, want)
		}
		if len(sp.Iterations) != 3 {
			t.Fatalf("concurrency %d: want 3 iterations, got %d", sp.Concurrency, len(sp.Iterations))
		}
		medians := 0
		for _, it := range sp.Iterations {
			if it.Median {
				medians++
				if got := newOrderTpm(it); got != wantMedian[sp.Concurrency] {
					t.Errorf("concurrency %d: median run tpm = %v, want %v", sp.Concurrency, got, wantMedian[sp.Concurrency])
				}
			}
			wantFile := fmt.Sprintf("raw-clients-%d-iteration-%d.csv", sp.Concurrency, it.Iteration)
			if it.RawMetricsFile == nil || *it.RawMetricsFile != wantFile {
				t.Errorf("concurrency %d iteration %d: raw_metrics_file = %v, want %q", sp.Concurrency, it.Iteration, it.RawMetricsFile, wantFile)
				continue
			}
			rows := readCSVRows(t, filepath.Join(scenarioDir, wantFile))
			if len(rows) != 3 { // header + 2 ticks
				t.Fatalf("%s: want 3 rows (header+2 ticks), got %d: %v", wantFile, len(rows), rows)
			}
			if want := fmt.Sprintf("%.1f", newOrderTpm(it)); rows[1][1] != want {
				t.Errorf("%s row[1].new_order_tpm = %q, want %q (this run's own data)", wantFile, rows[1][1], want)
			}
		}
		if medians != 1 {
			t.Errorf("concurrency %d: %d median runs, want exactly 1", sp.Concurrency, medians)
		}
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
	scenarioDir := filepath.Join(dir, "scenario")
	csvPath := filepath.Join(scenarioDir, "raw-clients-12-iteration-1.csv")

	firstDoc := buildTestDoc(t, candidates, scenarioDir)
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
	first, err := os.ReadFile(csvPath)
	if err != nil {
		t.Fatalf("read first output: %v", err)
	}
	buildTestDoc(t, candidates, scenarioDir)
	second, err := os.ReadFile(csvPath)
	if err != nil {
		t.Fatalf("read second output: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("output differs between runs:\n1st: %q\n2nd: %q", first, second)
	}
}

// TestBuildResultDoc_TimestampsComeFromStepWindows covers both sources of an
// iteration's started_at/completed_at: the benchmark step in
// step_windows.json when the run has one, the run's span otherwise.
func TestBuildResultDoc_TimestampsComeFromStepWindows(t *testing.T) {
	dir := t.TempDir()
	windows := `[{"step": "benchmark", "fixture": {"client_threads": "12"}, "started_at": "2026-09-30T14:03:35.87Z", "ended_at": "2026-09-30T15:03:37.36Z"}]`
	if err := os.MkdirAll(filepath.Join(dir, "run-a"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run-a", "step_windows.json"), []byte(windows), 0o644); err != nil {
		t.Fatalf("write step_windows.json: %v", err)
	}
	withWindows := makeCandidateRun(t, dir, "run-a", 1, "12", 1500.0, "", "")
	withoutWindows := makeCandidateRun(t, dir, "run-b", 2, "12", 1600.0, "", "")

	doc := buildTestDoc(t, []candidateRun{withWindows, withoutWindows}, filepath.Join(dir, "scenario"))
	its := doc.Sweep[0].Iterations
	if its[0].StartedAt != "2026-09-30T14:03:35Z" || its[0].CompletedAt != "2026-09-30T15:03:37Z" {
		t.Errorf("iteration 1 = %s..%s, want the benchmark step window", its[0].StartedAt, its[0].CompletedAt)
	}
	wantStart := withoutWindows.run.CreatedAt.UTC().Format(time.RFC3339)
	if its[1].StartedAt != wantStart {
		t.Errorf("iteration 2 started_at = %s, want the run's span %s", its[1].StartedAt, wantStart)
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

// TestBuildResultDoc_CarriesMetadataIntoEveryIteration checks that metadata
// records reach each run's iteration, keyed by record name.
func TestBuildResultDoc_CarriesMetadataIntoEveryIteration(t *testing.T) {
	dir := t.TempDir()
	rtt := metricRecord{FixtureThreads: "12", Name: "network_rtt",
		Value: "min_us=96, median_us=154, p99_us=299, max_us=337, samples=200"}
	candidates := []candidateRun{
		makeCandidateRun(t, dir, "run-a", 1, "12", 1500.0, "1.2.0", "latest", rtt),
		makeCandidateRun(t, dir, "run-b", 2, "12", 1600.0, "1.2.0", "latest"),
	}
	doc := buildTestDoc(t, candidates, filepath.Join(dir, "scenario"))

	its := doc.Sweep[0].Iterations
	blob, err := json.Marshal(its[0].Metadata)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	want := `{"network_rtt":{"max_us":337,"median_us":154,"min_us":96,"p99_us":299,"samples":200}}`
	if string(blob) != want {
		t.Errorf("iteration 1 metadata = %s, want %s", blob, want)
	}
	if len(its[1].Metadata) != 0 {
		t.Errorf("iteration 2 metadata = %v, want empty for a run without metadata records", its[1].Metadata)
	}
}

func TestPgSettingsInfo(t *testing.T) {
	records := newOrderMetricRecords("8", 1000, "v1", "v2")
	if got := pgSettingsInfo(records); got != "" {
		t.Errorf("pgSettingsInfo = %q, want empty for records with no pg_settings row", got)
	}

	records = append(records, metricRecord{FixtureThreads: "8", Name: "pg_settings",
		Value: "shared_buffers=12800, work_mem=5120"})
	if got := pgSettingsInfo(records); got != "shared_buffers=12800, work_mem=5120" {
		t.Errorf("pgSettingsInfo = %q, want the raw record value", got)
	}
}

func TestOrioledbVersionInfo(t *testing.T) {
	records := newOrderMetricRecords("8", 1000, "v1", "v2")
	if got := orioledbVersionInfo(records); got != "" {
		t.Errorf("orioledbVersionInfo = %q, want empty for a non-OrioleDB engine", got)
	}

	records = append(records, metricRecord{FixtureThreads: "8", Name: "orioledb_version", Value: "OrioleDB beta 17"})
	if got := orioledbVersionInfo(records); got != "OrioleDB beta 17" {
		t.Errorf("orioledbVersionInfo = %q, want %q", got, "OrioleDB beta 17")
	}
}

// TestBuildResultDoc_CarriesPgSettingsIntoTheInstance is the end-to-end
// check that benchctl's pg_settings/orioledb_version metadata records reach
// instance.
func TestBuildResultDoc_CarriesPgSettingsIntoTheInstance(t *testing.T) {
	dir := t.TempDir()
	extra := []metricRecord{
		{FixtureThreads: "12", Name: "pg_settings", Value: "shared_buffers=12800, work_mem=5120"},
		{FixtureThreads: "12", Name: "orioledb_version", Value: "OrioleDB beta 17"},
	}
	candidates := []candidateRun{
		makeCandidateRun(t, dir, "run-a", 1, "12", 1500.0, "1.2.0", "latest", extra...),
	}
	scenarioDir := filepath.Join(dir, "scenario")
	if err := os.MkdirAll(scenarioDir, 0o755); err != nil {
		t.Fatalf("mkdir scenarioDir: %v", err)
	}

	doc, err := buildResultDoc(resultDocInputs{
		Manifest:     &manifest.Manifest{Provider: "Supabase", Product: "OrioleDB", Workload: "tpcc"},
		TestPoint:    &sweepstate.TestPoint{Tier: "small", BoundType: "cache-fit", SweepID: "sweep-1"},
		Def:          &manifest.TestPointDef{Tier: "small", BoundType: "cache-fit"},
		Successful:   candidates,
		ManifestPath: "candidate.yaml",
		ScenarioDir:  scenarioDir,
	})
	if err != nil {
		t.Fatalf("buildResultDoc: %v", err)
	}

	if got := doc.Instance.PgSettings; got == nil || *got != "shared_buffers=12800, work_mem=5120" {
		t.Errorf("instance.pg_settings = %v, want the raw settings string", got)
	}
	if got := doc.Instance.OrioleDBVersion; got == nil || *got != "OrioleDB beta 17" {
		t.Errorf("instance.orioledb_version = %v, want %q", got, "OrioleDB beta 17")
	}
}

// TestBuildResultDoc_OmitsOrioleDBVersionForOtherEngines asserts
// orioledb_version is dropped from the marshaled instance JSON entirely
// (not written as null) when the engine under test isn't OrioleDB.
func TestBuildResultDoc_OmitsOrioleDBVersionForOtherEngines(t *testing.T) {
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
	if doc.Instance.OrioleDBVersion != nil {
		t.Errorf("instance.orioledb_version = %v, want nil", *doc.Instance.OrioleDBVersion)
	}

	blob, err := json.Marshal(doc.Instance)
	if err != nil {
		t.Fatalf("marshal instance: %v", err)
	}
	if strings.Contains(string(blob), "orioledb_version") {
		t.Errorf("instance JSON should omit orioledb_version, got: %s", blob)
	}
	if strings.Contains(string(blob), "pg_settings") {
		t.Errorf("instance JSON should omit pg_settings, got: %s", blob)
	}
}

// TestBuildResultDoc_SupabaseReadsVCPURAMFromDiagnostics asserts Supabase's
// vcpu/ram_gb come from the run's own diagnostics/addons.json -- not from a
// pricing snapshot, which this test deliberately leaves nil to prove the
// diagnostics path doesn't depend on one.
func TestBuildResultDoc_SupabaseReadsVCPURAMFromDiagnostics(t *testing.T) {
	dir := t.TempDir()
	candidates := []candidateRun{makeCandidateRun(t, dir, "run-a", 1, "12", 1500.0, "1.2.0", "latest")}
	writeSupabaseAddonsFixture(t, filepath.Join(dir, "run-a"), 8, 32)
	scenarioDir := filepath.Join(dir, "scenario")
	if err := os.MkdirAll(scenarioDir, 0o755); err != nil {
		t.Fatalf("mkdir scenarioDir: %v", err)
	}

	doc, err := buildResultDoc(resultDocInputs{
		Manifest:     &manifest.Manifest{Provider: "Supabase", Product: "OrioleDB", Workload: "tpcc"},
		TestPoint:    &sweepstate.TestPoint{Tier: "2xlarge", BoundType: "cache-fit", SweepID: "sweep-1"},
		Def:          &manifest.TestPointDef{Tier: "2xlarge", BoundType: "cache-fit", Set: map[string]string{"project_size": "2xlarge"}},
		Successful:   candidates,
		Snapshot:     nil,
		ManifestPath: "candidate.yaml",
		ScenarioDir:  scenarioDir,
	})
	if err != nil {
		t.Fatalf("buildResultDoc: %v", err)
	}

	if got := doc.Instance.VCPU; got == nil || *got != 8 {
		t.Errorf("instance.vcpu = %v, want 8", got)
	}
	if got := doc.Instance.RAMGB; got == nil || *got != 32 {
		t.Errorf("instance.ram_gb = %v, want 32", got)
	}
}

// TestBuildResultDoc_SupabaseLeavesVCPURAMNilWithoutDiagnostics asserts a
// Supabase run missing diagnostics/addons.json (e.g. an older artifact)
// still produces a result, just with vcpu/ram_gb left null rather than
// falling back to any pricing-derived guess.
func TestBuildResultDoc_SupabaseLeavesVCPURAMNilWithoutDiagnostics(t *testing.T) {
	dir := t.TempDir()
	candidates := []candidateRun{makeCandidateRun(t, dir, "run-a", 1, "12", 1500.0, "1.2.0", "latest")}
	scenarioDir := filepath.Join(dir, "scenario")
	if err := os.MkdirAll(scenarioDir, 0o755); err != nil {
		t.Fatalf("mkdir scenarioDir: %v", err)
	}

	doc, err := buildResultDoc(resultDocInputs{
		Manifest:     &manifest.Manifest{Provider: "Supabase", Product: "OrioleDB", Workload: "tpcc"},
		TestPoint:    &sweepstate.TestPoint{Tier: "2xlarge", BoundType: "cache-fit", SweepID: "sweep-1"},
		Def:          &manifest.TestPointDef{Tier: "2xlarge", BoundType: "cache-fit", Set: map[string]string{"project_size": "2xlarge"}},
		Successful:   candidates,
		ManifestPath: "candidate.yaml",
		ScenarioDir:  scenarioDir,
	})
	if err != nil {
		t.Fatalf("buildResultDoc: %v", err)
	}

	if doc.Instance.VCPU != nil {
		t.Errorf("instance.vcpu = %v, want nil", *doc.Instance.VCPU)
	}
	if doc.Instance.RAMGB != nil {
		t.Errorf("instance.ram_gb = %v, want nil", *doc.Instance.RAMGB)
	}
}

// TestBuildResultDoc_SupabaseDiskTypeFromDiagnosticsWinsOverManifest asserts
// instance.disk_type comes from the run's own diagnostics/disk.json when
// present, even when the manifest declares a different value -- diagnostics
// is the ground truth, the manifest's declared value is only a fallback.
func TestBuildResultDoc_SupabaseDiskTypeFromDiagnosticsWinsOverManifest(t *testing.T) {
	dir := t.TempDir()
	candidates := []candidateRun{makeCandidateRun(t, dir, "run-a", 1, "12", 1500.0, "1.2.0", "latest")}
	writeSupabaseDiskFixture(t, filepath.Join(dir, "run-a"), "io2")
	scenarioDir := filepath.Join(dir, "scenario")
	if err := os.MkdirAll(scenarioDir, 0o755); err != nil {
		t.Fatalf("mkdir scenarioDir: %v", err)
	}

	doc, err := buildResultDoc(resultDocInputs{
		Manifest:     &manifest.Manifest{Provider: "Supabase", Product: "OrioleDB", Workload: "tpcc"},
		TestPoint:    &sweepstate.TestPoint{Tier: "2xlarge", BoundType: "cache-fit", SweepID: "sweep-1"},
		Def:          &manifest.TestPointDef{Tier: "2xlarge", BoundType: "cache-fit", Set: map[string]string{"project_size": "2xlarge", "disk_type": "gp3"}},
		Successful:   candidates,
		ManifestPath: "candidate.yaml",
		ScenarioDir:  scenarioDir,
	})
	if err != nil {
		t.Fatalf("buildResultDoc: %v", err)
	}

	if got := doc.Instance.DiskType; got == nil || *got != "io2" {
		t.Errorf("instance.disk_type = %v, want %q (diagnostics, not the manifest's %q)", got, "io2", "gp3")
	}
}

// TestBuildResultDoc_SupabaseDiskTypeFallsBackToManifestWithoutDiagnostics
// asserts instance.disk_type falls back to the manifest's declared
// disk_type when a run has no diagnostics/disk.json.
func TestBuildResultDoc_SupabaseDiskTypeFallsBackToManifestWithoutDiagnostics(t *testing.T) {
	dir := t.TempDir()
	candidates := []candidateRun{makeCandidateRun(t, dir, "run-a", 1, "12", 1500.0, "1.2.0", "latest")}
	scenarioDir := filepath.Join(dir, "scenario")
	if err := os.MkdirAll(scenarioDir, 0o755); err != nil {
		t.Fatalf("mkdir scenarioDir: %v", err)
	}

	doc, err := buildResultDoc(resultDocInputs{
		Manifest:     &manifest.Manifest{Provider: "Supabase", Product: "OrioleDB", Workload: "tpcc"},
		TestPoint:    &sweepstate.TestPoint{Tier: "2xlarge", BoundType: "cache-fit", SweepID: "sweep-1"},
		Def:          &manifest.TestPointDef{Tier: "2xlarge", BoundType: "cache-fit", Set: map[string]string{"project_size": "2xlarge", "disk_type": "gp3"}},
		Successful:   candidates,
		ManifestPath: "candidate.yaml",
		ScenarioDir:  scenarioDir,
	})
	if err != nil {
		t.Fatalf("buildResultDoc: %v", err)
	}

	if got := doc.Instance.DiskType; got == nil || *got != "gp3" {
		t.Errorf("instance.disk_type = %v, want %q (the manifest's configured value)", got, "gp3")
	}
}

// writeSupabaseDiskFixture writes a diagnostics/disk.json under runDir
// shaped like benchctl's real GET .../config/disk snapshot.
func writeSupabaseDiskFixture(t *testing.T, runDir, diskType string) {
	t.Helper()
	diagDir := filepath.Join(runDir, "diagnostics")
	if err := os.MkdirAll(diagDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", diagDir, err)
	}
	body := fmt.Sprintf(`{"attributes":{"iops":12000,"size_gb":64,"throughput_mibps":300,"type":%q},"last_modified_at":"2026-09-30T09:48:45.950Z"}`, diskType)
	if err := os.WriteFile(filepath.Join(diagDir, "disk.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write disk.json: %v", err)
	}
}

// writeSupabaseAddonsFixture writes a diagnostics/addons.json under runDir
// shaped like benchctl's real GET .../billing/addons snapshot (trimmed to
// selected_addons, per its diagnostics collector).
func writeSupabaseAddonsFixture(t *testing.T, runDir string, cpuCores, memoryGB int) {
	t.Helper()
	diagDir := filepath.Join(runDir, "diagnostics")
	if err := os.MkdirAll(diagDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", diagDir, err)
	}
	body := fmt.Sprintf(`{
  "selected_addons": [
    {
      "type": "compute_instance",
      "variant": {
        "id": "ci_2xlarge",
        "name": "2XL",
        "meta": {
          "cpu_cores": %d,
          "cpu_dedicated": true,
          "memory_gb": %d
        }
      }
    }
  ]
}`, cpuCores, memoryGB)
	if err := os.WriteFile(filepath.Join(diagDir, "addons.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write addons.json: %v", err)
	}
}

func TestSupabaseComputeSize_MissingFile(t *testing.T) {
	vcpu, ramGB := supabaseComputeSize(t.TempDir())
	if vcpu != nil || ramGB != nil {
		t.Errorf("vcpu=%v ramGB=%v, want nil, nil for a missing diagnostics file", vcpu, ramGB)
	}
}

func TestSupabaseComputeSize_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	diagDir := filepath.Join(dir, "diagnostics")
	if err := os.MkdirAll(diagDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", diagDir, err)
	}
	if err := os.WriteFile(filepath.Join(diagDir, "addons.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write addons.json: %v", err)
	}

	vcpu, ramGB := supabaseComputeSize(dir)
	if vcpu != nil || ramGB != nil {
		t.Errorf("vcpu=%v ramGB=%v, want nil, nil for malformed JSON", vcpu, ramGB)
	}
}

func TestSupabaseComputeSize_NoComputeInstanceEntry(t *testing.T) {
	dir := t.TempDir()
	diagDir := filepath.Join(dir, "diagnostics")
	if err := os.MkdirAll(diagDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", diagDir, err)
	}
	body := `{"selected_addons": [{"type": "pitr", "variant": {"id": "pitr_7"}}]}`
	if err := os.WriteFile(filepath.Join(diagDir, "addons.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write addons.json: %v", err)
	}

	vcpu, ramGB := supabaseComputeSize(dir)
	if vcpu != nil || ramGB != nil {
		t.Errorf("vcpu=%v ramGB=%v, want nil, nil when no compute_instance addon is selected", vcpu, ramGB)
	}
}

func TestSupabaseComputeSize_RealPayloadShape(t *testing.T) {
	dir := t.TempDir()
	writeSupabaseAddonsFixture(t, dir, 8, 32)

	vcpu, ramGB := supabaseComputeSize(dir)
	if vcpu == nil || *vcpu != 8 {
		t.Errorf("vcpu = %v, want 8", vcpu)
	}
	if ramGB == nil || *ramGB != 32 {
		t.Errorf("ramGB = %v, want 32", ramGB)
	}
}

func TestSupabaseDiskType_MissingFile(t *testing.T) {
	if got := supabaseDiskType(t.TempDir()); got != nil {
		t.Errorf("diskType = %v, want nil for a missing diagnostics file", *got)
	}
}

func TestSupabaseDiskType_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	diagDir := filepath.Join(dir, "diagnostics")
	if err := os.MkdirAll(diagDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", diagDir, err)
	}
	if err := os.WriteFile(filepath.Join(diagDir, "disk.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write disk.json: %v", err)
	}

	if got := supabaseDiskType(dir); got != nil {
		t.Errorf("diskType = %v, want nil for malformed JSON", *got)
	}
}

func TestSupabaseDiskType_RealPayloadShape(t *testing.T) {
	dir := t.TempDir()
	writeSupabaseDiskFixture(t, dir, "gp3")

	got := supabaseDiskType(dir)
	if got == nil || *got != "gp3" {
		t.Errorf("diskType = %v, want %q", got, "gp3")
	}
}

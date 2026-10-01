package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/dbarena/dbarenactl/internal/sweepstate"
)

// TestMetricRecord_UnmarshalsFixtureClientThreads guards against a repeat of
// the fixture_threads/fixture_client_threads tag mismatch: benchctl emits
// fixture_client_threads, and a struct-literal test wouldn't catch a wrong
// json tag the way an actual json.Unmarshal does.
func TestMetricRecord_UnmarshalsFixtureClientThreads(t *testing.T) {
	const data = `{"fixture_client_threads": "12", "name": "tpcc_tpm", "value": 100}`
	var r metricRecord
	if err := json.Unmarshal([]byte(data), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.FixtureThreads != "12" {
		t.Errorf("FixtureThreads = %q, want %q", r.FixtureThreads, "12")
	}
}

// TestLoadRunMetrics_SkipsWarmupStepRecords confirms that only step=="warmup"
// records are excluded -- step=="benchmark" and step=="" (older data
// predating the warmup/benchmark split) both survive into byThreads.
func TestLoadRunMetrics_SkipsWarmupStepRecords(t *testing.T) {
	dir := t.TempDir()
	records := []metricRecord{
		{FixtureThreads: "8", Name: "tpcc_tpm", Transaction: "NEW_ORDER", Status: "ok", Step: "warmup", Value: 999.0},
		{FixtureThreads: "8", Name: "tpcc_tpm", Transaction: "NEW_ORDER", Status: "ok", Step: "benchmark", Value: 100.0},
		{FixtureThreads: "8", Name: "tpcc_count", Transaction: "NEW_ORDER", Status: "ok", Value: 1000.0},
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "results_supabase_1_8.json"), data, 0o644); err != nil {
		t.Fatalf("write results json: %v", err)
	}

	byThreads, _, err := loadRunMetrics(dir)
	if err != nil {
		t.Fatalf("loadRunMetrics: %v", err)
	}

	got := byThreads[8]
	if len(got) != 2 {
		t.Fatalf("byThreads[8] has %d records, want 2 (warmup record excluded): %+v", len(got), got)
	}
	for _, r := range got {
		if r.Step == "warmup" {
			t.Errorf("warmup record leaked into byThreads: %+v", r)
		}
	}
	tpm, err := tpmAt(got)
	if err != nil {
		t.Fatalf("tpmAt: %v", err)
	}
	if tpm != 100.0 {
		t.Errorf("tpmAt = %v, want 100.0 (the benchmark-step record, not the warmup-step one)", tpm)
	}
}

// metadataJSON marshals metadataFrom's output so tests compare against the
// JSON that actually lands in result.json.
func metadataJSON(t *testing.T, records []metricRecord) string {
	t.Helper()
	metadata, err := metadataFrom(records)
	if err != nil {
		t.Fatalf("metadataFrom: %v", err)
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	return string(data)
}

func TestMetadataFrom_ParsesKeyValueStrings(t *testing.T) {
	got := metadataJSON(t, []metricRecord{
		{Name: "network_rtt", Value: "min_us=96, median_us=154, samples=200"},
		{Name: "db_size_before", Value: "db_bytes=2011349183,waldir_bytes=1077936128"},
		{Name: "pg_settings", Value: "checkpoint_completion_target=0.90, synchronous_commit=on"},
	})
	want := `{"db_size_before":{"db_bytes":2011349183,"waldir_bytes":1077936128},` +
		`"network_rtt":{"median_us":154,"min_us":96,"samples":200},` +
		`"pg_settings":{"checkpoint_completion_target":0.90,"synchronous_commit":"on"}}`
	if got != want {
		t.Errorf("metadata =\n%s\nwant\n%s", got, want)
	}
}

func TestMetadataFrom_KeepsOtherStringsVerbatim(t *testing.T) {
	version := "PostgreSQL 17.11 on aarch64-unknown-linux-gnu, compiled by gcc"
	got := metadataJSON(t, []metricRecord{
		{Name: "pg_version", Value: version},
		{Name: "half_pairs", Value: "a=1, b"},
	})
	want := `{"half_pairs":"a=1, b","pg_version":"` + version + `"}`
	if got != want {
		t.Errorf("metadata = %s, want %s", got, want)
	}
}

func TestMetadataFrom_NestsNumbersUnderTheirLabels(t *testing.T) {
	got := metadataJSON(t, []metricRecord{
		{Name: "driver_cpu_utilization", Quantile: "0.99", Value: 0.25},
		{Name: "driver_cpu_utilization", Quantile: "0.999", Value: 0.3},
		{Name: "driver_network_throughput_bytes_per_sec", Direction: "receive", Quantile: "0.99", Value: 1000.0},
		{Name: "driver_network_throughput_bytes_per_sec", Direction: "transmit", Quantile: "0.99", Value: 2000.0},
		{Name: "unlabelled", Value: 7.0},
	})
	want := `{"driver_cpu_utilization":{"0.99":0.25,"0.999":0.3},` +
		`"driver_network_throughput_bytes_per_sec":{"receive":{"0.99":1000},"transmit":{"0.99":2000}},` +
		`"unlabelled":7}`
	if got != want {
		t.Errorf("metadata =\n%s\nwant\n%s", got, want)
	}
}

func TestMetadataFrom_SkipsMeasurements(t *testing.T) {
	records := append(newOrderMetricRecords("8", 1000, "v1", "v2"),
		metricRecord{FixtureThreads: "8", Name: "wal_size_after", Value: "wal_bytes=1077881251"})
	if got, want := metadataJSON(t, records), `{"wal_size_after":{"wal_bytes":1077881251}}`; got != want {
		t.Errorf("metadata = %s, want %s", got, want)
	}
}

func TestMetadataFrom_RejectsDuplicatePaths(t *testing.T) {
	for name, records := range map[string][]metricRecord{
		"same record twice": {
			{Name: "network_rtt", Value: "min_us=1"},
			{Name: "network_rtt", Value: "min_us=2"},
		},
		"value and labelled value": {
			{Name: "driver_cpu_utilization", Value: 0.1},
			{Name: "driver_cpu_utilization", Quantile: "0.99", Value: 0.2},
		},
	} {
		if _, err := metadataFrom(records); err == nil {
			t.Errorf("%s: metadataFrom succeeded, want a duplicate-path error", name)
		}
	}
}

func TestLoadStepWindows_KeysBenchmarkStepsByClientThreads(t *testing.T) {
	dir := t.TempDir()
	data := `[
  {"step": "warmup", "fixture": {"client_threads": "8"}, "started_at": "2026-09-30T12:02:23Z", "ended_at": "2026-09-30T12:32:25Z"},
  {"step": "benchmark", "fixture": {"client_threads": "8"}, "started_at": "2026-09-30T12:32:27.2244Z", "ended_at": "2026-09-30T13:32:29.1820Z"},
  {"step": "benchmark", "fixture": {"client_threads": "16"}, "started_at": "2026-09-30T14:03:35Z", "ended_at": "2026-09-30T15:03:37Z"}
]`
	if err := os.WriteFile(filepath.Join(dir, "step_windows.json"), []byte(data), 0o644); err != nil {
		t.Fatalf("write step_windows.json: %v", err)
	}

	windows, err := loadStepWindows(dir)
	if err != nil {
		t.Fatalf("loadStepWindows: %v", err)
	}
	if len(windows) != 2 {
		t.Fatalf("want 2 windows (benchmark steps only), got %d: %v", len(windows), windows)
	}
	want := time.Date(2026, 9, 30, 12, 32, 27, 224400000, time.UTC)
	if got := windows[8].StartedAt; !got.Equal(want) {
		t.Errorf("windows[8].StartedAt = %v, want %v (the benchmark step, not warmup)", got, want)
	}
	if got := windows[16].EndedAt; !got.Equal(time.Date(2026, 9, 30, 15, 3, 37, 0, time.UTC)) {
		t.Errorf("windows[16].EndedAt = %v", got)
	}
}

func TestLoadStepWindows_MissingFileYieldsNil(t *testing.T) {
	windows, err := loadStepWindows(t.TempDir())
	if err != nil || windows != nil {
		t.Errorf("loadStepWindows = %v, %v; want nil, nil", windows, err)
	}
}

// runWithTpm builds an in-memory candidate run with the given NEW_ORDER tpm
// per concurrency level.
func runWithTpm(runID string, iteration int, tpmByThreads map[int]float64) candidateRun {
	metrics := map[int][]metricRecord{}
	for threads, tpm := range tpmByThreads {
		metrics[threads] = newOrderMetricRecords(strconv.Itoa(threads), tpm, "", "")
	}
	return candidateRun{
		run:              &sweepstate.Run{RunID: runID, IterationAttempt: iteration},
		metricsByThreads: metrics,
	}
}

// TestSelectRuns_MethodologyExample uses the worked example from the
// methodology: runs peak at different client counts, and the peak is where
// the median is highest.
func TestSelectRuns_MethodologyExample(t *testing.T) {
	runs := []candidateRun{
		runWithTpm("run-1", 1, map[int]float64{4: 9302, 8: 15529, 16: 18085, 24: 7647}),
		runWithTpm("run-2", 2, map[int]float64{4: 9395, 8: 14846, 16: 16200, 24: 7228}),
		runWithTpm("run-3", 3, map[int]float64{4: 10948, 8: 16300, 16: 11852, 24: 8079}),
	}
	peak, median, err := selectRuns(runs)
	if err != nil {
		t.Fatalf("selectRuns: %v", err)
	}
	if peak != 16 {
		t.Errorf("peak = %d, want 16", peak)
	}
	for threads, wantRun := range map[int]string{4: "run-2", 8: "run-1", 16: "run-2", 24: "run-1"} {
		if got := median[threads].run.RunID; got != wantRun {
			t.Errorf("median[%d] = %s, want %s", threads, got, wantRun)
		}
	}
}

func TestSelectRuns_TieGoesToLowerConcurrency(t *testing.T) {
	runs := []candidateRun{runWithTpm("run-1", 1, map[int]float64{8: 1000, 16: 1000})}
	peak, _, err := selectRuns(runs)
	if err != nil {
		t.Fatalf("selectRuns: %v", err)
	}
	if peak != 8 {
		t.Errorf("peak = %d, want 8", peak)
	}
}

func TestSelectRuns_EvenRunCountPicksLowerMedian(t *testing.T) {
	runs := []candidateRun{
		runWithTpm("run-1", 1, map[int]float64{8: 2000}),
		runWithTpm("run-2", 2, map[int]float64{8: 1000}),
	}
	_, median, err := selectRuns(runs)
	if err != nil {
		t.Fatalf("selectRuns: %v", err)
	}
	if got := median[8].run.RunID; got != "run-2" {
		t.Errorf("median[8] = %s, want run-2", got)
	}
}

func TestSelectRuns_LevelMissingFromARunCannotBeThePeak(t *testing.T) {
	runs := []candidateRun{
		runWithTpm("run-1", 1, map[int]float64{8: 1000, 16: 5000}),
		runWithTpm("run-2", 2, map[int]float64{8: 1100}),
	}
	peak, median, err := selectRuns(runs)
	if err != nil {
		t.Fatalf("selectRuns: %v", err)
	}
	if peak != 8 {
		t.Errorf("peak = %d, want 8", peak)
	}
	if got := median[16].run.RunID; got != "run-1" {
		t.Errorf("median[16] = %s, want run-1 (still reported)", got)
	}
}

func TestSelectRuns_NoCommonLevelIsAnError(t *testing.T) {
	runs := []candidateRun{
		runWithTpm("run-1", 1, map[int]float64{8: 1000}),
		runWithTpm("run-2", 2, map[int]float64{16: 1000}),
	}
	if _, _, err := selectRuns(runs); err == nil {
		t.Error("selectRuns succeeded, want an error")
	}
}

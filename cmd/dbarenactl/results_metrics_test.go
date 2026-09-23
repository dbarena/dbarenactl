package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
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

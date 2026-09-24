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

// driverMetricRecords builds a full, valid set of
// driver_cpu_utilization/driver_network_throughput_bytes_per_sec records
// (three quantiles each, two directions for network) for use as a base
// fixture in the loadDriverInfoFrom tests below.
func driverMetricRecords() []metricRecord {
	var records []metricRecord
	cpu := map[string]float64{"0.99": 0.25, "0.999": 0.3, "0.9999": 0.35}
	for q, v := range cpu {
		records = append(records, metricRecord{Name: "driver_cpu_utilization", Quantile: q, Value: v})
	}
	network := map[string]float64{"0.99": 1000, "0.999": 2000, "0.9999": 3000}
	for _, direction := range []string{"receive", "transmit"} {
		for q, v := range network {
			records = append(records, metricRecord{
				Name: "driver_network_throughput_bytes_per_sec", Quantile: q, Direction: direction, Value: v,
			})
		}
	}
	return records
}

func TestLoadDriverInfoFrom_HappyPath(t *testing.T) {
	got := loadDriverInfoFrom(driverMetricRecords())
	if got == nil {
		t.Fatal("loadDriverInfoFrom = nil, want a populated block")
	}
	want := loadDriverInfo{
		CPUUtilization: percentileInfo{P99: 0.25, P999: 0.3, P9999: 0.35},
		NetworkThroughputBytesPerSec: networkThroughputInfo{
			Receive:  percentileInfo{P99: 1000, P999: 2000, P9999: 3000},
			Transmit: percentileInfo{P99: 1000, P999: 2000, P9999: 3000},
		},
	}
	if *got != want {
		t.Errorf("loadDriverInfoFrom = %+v, want %+v", *got, want)
	}
}

func TestLoadDriverInfoFrom_MissingCPUQuantile_ReturnsNil(t *testing.T) {
	records := driverMetricRecords()
	for i, r := range records {
		if r.Name == "driver_cpu_utilization" && r.Quantile == "0.9999" {
			records = append(records[:i], records[i+1:]...)
			break
		}
	}
	if got := loadDriverInfoFrom(records); got != nil {
		t.Errorf("loadDriverInfoFrom = %+v, want nil (missing a CPU quantile)", *got)
	}
}

func TestLoadDriverInfoFrom_MissingNetworkDirection_ReturnsNil(t *testing.T) {
	var records []metricRecord
	for _, r := range driverMetricRecords() {
		if r.Name == "driver_network_throughput_bytes_per_sec" && r.Direction == "transmit" {
			continue
		}
		records = append(records, r)
	}
	if got := loadDriverInfoFrom(records); got != nil {
		t.Errorf("loadDriverInfoFrom = %+v, want nil (transmit direction entirely missing)", *got)
	}
}

func TestLoadDriverInfoFrom_NoDriverRecords_ReturnsNil(t *testing.T) {
	records := []metricRecord{{Name: "tpcc_tpm", Value: 100.0}}
	if got := loadDriverInfoFrom(records); got != nil {
		t.Errorf("loadDriverInfoFrom = %+v, want nil (no driver_* records at all)", *got)
	}
}

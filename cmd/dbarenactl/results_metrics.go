package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/dbarena/dbarenactl/internal/sweepstate"
)

// metricRecord is one entry in a run's runs/<run-id>/results_<benchmark>_<const>_<threads>.json
// file, as written by go-tpc's metrics collector. Value is `any` because
// metadata records (pg_version/pg_settings) carry a string, while every
// other record carries a number.
type metricRecord struct {
	Benchmark       string `json:"benchmark"`
	FixtureThreads  string `json:"fixture_client_threads"`
	Iteration       int    `json:"iteration"`
	Name            string `json:"name"`
	ProjectID       string `json:"project_id"`
	Service         string `json:"service"`
	Status          string `json:"status,omitempty"`
	Transaction     string `json:"transaction,omitempty"`
	Quantile        string `json:"quantile,omitempty"`
	Step            string `json:"step,omitempty"`
	Value           any    `json:"value"`
	BenchctlVersion string `json:"benchctl_version,omitempty"`
	GotpcVersion    string `json:"gotpc_version,omitempty"`
}

func (m metricRecord) floatValue() (float64, error) {
	switch v := m.Value.(type) {
	case float64:
		return v, nil
	case string:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, fmt.Errorf("metric %s/%s: value %q is not numeric", m.Name, m.Transaction, v)
		}
		return f, nil
	default:
		return 0, fmt.Errorf("metric %s/%s: unexpected value type %T", m.Name, m.Transaction, v)
	}
}

func (m metricRecord) stringValue() (string, error) {
	s, ok := m.Value.(string)
	if !ok {
		return "", fmt.Errorf("metric %s: value is not a string", m.Name)
	}
	return s, nil
}

// loadRunMetrics reads and flattens every results_*.json file directly
// inside artifactDir into one slice, dropping any record whose step is
// "warmup" (older records without step field are kept, since they predate
// the warmup/benchmark split and are benchmark-phase data), then groups the
// remaining records by their fixture_client_threads (i.e. concurrency)
// value. It also associates each concurrency with its sibling
// raw_samples_*.csv path, when one exists.
// results_<X>.json and raw_samples_<X>.csv are always written by the same
// benchctl Collect() call with the same naming inputs, so the raw-samples
// path is derived from the results path found here (see rawSamplesPathFor)
// rather than independently re-parsed from its own filename.
func loadRunMetrics(artifactDir string) (map[int][]metricRecord, map[int]string, error) {
	matches, err := filepath.Glob(filepath.Join(artifactDir, "results_*.json"))
	if err != nil {
		return nil, nil, fmt.Errorf("glob %s: %w", artifactDir, err)
	}
	byThreads := map[int][]metricRecord{}
	rawSamplesByThreads := map[int]string{}
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", path, err)
		}
		var records []metricRecord
		if err := json.Unmarshal(data, &records); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", path, err)
		}
		var threads int
		haveThreads := false
		for _, r := range records {
			if r.Step == "warmup" {
				continue
			}
			t, err := strconv.Atoi(r.FixtureThreads)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: fixture_client_threads %q is not an integer: %w", path, r.FixtureThreads, err)
			}
			if !haveThreads {
				threads = t
				haveThreads = true
			}
			byThreads[t] = append(byThreads[t], r)
		}
		if !haveThreads {
			continue
		}
		rawPath := rawSamplesPathFor(path)
		if _, err := os.Stat(rawPath); err == nil {
			rawSamplesByThreads[threads] = rawPath
		}
	}
	return byThreads, rawSamplesByThreads, nil
}

// candidateRun is one successful, artifact-bearing run being considered as
// the representative iteration for a test point.
type candidateRun struct {
	run                 *sweepstate.Run
	metricsByThreads    map[int][]metricRecord
	rawSamplesByThreads map[int]string
}

// tpmAt returns the primary-metric (tpm for NEW_ORDER, status=ok) throughput
// value at a given concurrency level.
func tpmAt(records []metricRecord) (float64, error) {
	for _, r := range records {
		if r.Name == "tpcc_tpm" && r.Transaction == "NEW_ORDER" && r.Status == "ok" {
			return r.floatValue()
		}
	}
	return 0, fmt.Errorf("no tpcc_tpm/NEW_ORDER/ok record found")
}

// selectRepresentativeRun picks the single run whose peak-concurrency
// throughput is the median among candidates, per the documented policy: no
// pooling/intermingling across a test point's independent iterations --
// every field in the result comes from exactly one chosen run.
//
// The peak concurrency compared is the highest fixture_threads value common
// to every candidate (a run missing data at that level can't be compared
// there and is excluded from consideration, but doesn't lower the peak
// itself). For an even number of candidates, the lower-median is chosen.
func selectRepresentativeRun(candidates []candidateRun) (candidateRun, int, error) {
	if len(candidates) == 0 {
		return candidateRun{}, 0, fmt.Errorf("no candidate runs to select from")
	}

	common := map[int]bool{}
	for threads := range candidates[0].metricsByThreads {
		common[threads] = true
	}
	for _, c := range candidates[1:] {
		for threads := range common {
			if _, ok := c.metricsByThreads[threads]; !ok {
				delete(common, threads)
			}
		}
	}
	peak := -1
	for threads := range common {
		if threads > peak {
			peak = threads
		}
	}
	if peak == -1 {
		return candidateRun{}, 0, fmt.Errorf("no concurrency level is common to all %d candidate runs", len(candidates))
	}

	type scored struct {
		c   candidateRun
		tpm float64
	}
	var scoredRuns []scored
	for _, c := range candidates {
		tpm, err := tpmAt(c.metricsByThreads[peak])
		if err != nil {
			return candidateRun{}, 0, fmt.Errorf("run %s at concurrency %d: %w", c.run.RunID, peak, err)
		}
		scoredRuns = append(scoredRuns, scored{c, tpm})
	}
	sort.Slice(scoredRuns, func(i, j int) bool { return scoredRuns[i].tpm < scoredRuns[j].tpm })
	median := scoredRuns[(len(scoredRuns)-1)/2]
	return median.c, peak, nil
}

// txnNames lists transaction types in the order the schema's example data presents them.
var txnNames = []string{"NEW_ORDER", "PAYMENT", "ORDER_STATUS", "DELIVERY", "STOCK_LEVEL"}

// buildTxnMetrics assembles one sweep point's workload_metrics.transactions
// map and errors map from that concurrency level's raw metric records.
func buildTxnMetrics(records []metricRecord) (map[string]txnMetrics, map[string]float64, error) {
	txns := map[string]txnMetrics{}
	errs := map[string]float64{}

	for _, name := range txnNames {
		var count, tpm float64
		var lat txnLatencyInfo
		found := false
		for _, r := range records {
			if r.Transaction != name || r.Status != "ok" {
				continue
			}
			found = true
			switch r.Name {
			case "tpcc_count":
				v, err := r.floatValue()
				if err != nil {
					return nil, nil, err
				}
				count = v
			case "tpcc_tpm":
				v, err := r.floatValue()
				if err != nil {
					return nil, nil, err
				}
				tpm = v
			case "tpcc_latency_ms":
				v, err := r.floatValue()
				if err != nil {
					return nil, nil, err
				}
				switch r.Quantile {
				case "p50":
					lat.P50 = v
				case "p90":
					lat.P90 = v
				case "p95":
					lat.P95 = v
				case "p99":
					lat.P99 = v
				case "p99_9":
					lat.P999 = v
				case "avg":
					lat.Avg = v
				case "max":
					lat.Max = v
				}
			}
		}
		if found {
			txns[name] = txnMetrics{Count: count, Tpm: tpm, LatencyMs: lat}
		}

		// An "error" status tpcc_tpm record is the workload's _ERR
		// counterpart metric for this transaction (go-tpc's own
		// NAME_ERR TPM counter) -- matches every checked-in example's
		// error figures, which are all TPM-shaped (fractional) rather
		// than raw counts. go-tpc emits this record for every
		// transaction on every run regardless of whether any errors
		// occurred, so only list it here when actually nonzero -- a
		// sparse map naming just the transactions that had real errors
		// is far more useful to a reader than one padded with zeros.
		for _, r := range records {
			if r.Transaction != name || r.Status != "error" || r.Name != "tpcc_tpm" {
				continue
			}
			v, err := r.floatValue()
			if err != nil {
				return nil, nil, err
			}
			if v > 0 {
				errs[name] = v
			}
			break
		}
	}
	return txns, errs, nil
}

// latencyFor returns the p50/p95/p99 latency for one transaction at a
// concurrency level, for the schema's top-level summary.latency_ms.
func latencyFor(records []metricRecord, transaction string) (p50, p95, p99 float64, err error) {
	for _, r := range records {
		if r.Name != "tpcc_latency_ms" || r.Transaction != transaction || r.Status != "ok" {
			continue
		}
		v, ferr := r.floatValue()
		if ferr != nil {
			return 0, 0, 0, ferr
		}
		switch r.Quantile {
		case "p50":
			p50 = v
		case "p95":
			p95 = v
		case "p99":
			p99 = v
		}
	}
	return p50, p95, p99, nil
}

// pgVersionInfo returns (full version string, cpu_arch) parsed from a
// pg_version metadata record, if present at this concurrency level. cpu_arch
// is a substring match against the two architectures benchctl's driver
// fleet actually uses -- an unrecognized string just leaves cpu_arch unset
// rather than guessing.
func pgVersionInfo(records []metricRecord) (version, cpuArch string) {
	for _, r := range records {
		if r.Name != "pg_version" {
			continue
		}
		v, err := r.stringValue()
		if err != nil {
			continue
		}
		version = v
		switch {
		case strings.Contains(v, "aarch64"), strings.Contains(v, "arm64"):
			cpuArch = "aarch64"
		case strings.Contains(v, "x86_64"):
			cpuArch = "x86_64"
		}
		return version, cpuArch
	}
	return "", ""
}

// pgSettingsInfo returns the raw value of a pg_settings metadata record, if
// present at this concurrency level -- one flat, comma-separated
// "key=value" string exactly as benchctl captured it, not parsed further.
func pgSettingsInfo(records []metricRecord) string {
	for _, r := range records {
		if r.Name != "pg_settings" {
			continue
		}
		v, err := r.stringValue()
		if err != nil {
			continue
		}
		return v
	}
	return ""
}

// orioledbVersionInfo returns the raw value of an orioledb_version metadata
// record, if present. benchctl only emits this record when the engine
// under test is OrioleDB.
func orioledbVersionInfo(records []metricRecord) string {
	for _, r := range records {
		if r.Name != "orioledb_version" {
			continue
		}
		v, err := r.stringValue()
		if err != nil {
			continue
		}
		return v
	}
	return ""
}

// sizeBytesInfo returns the integer byte count from a recordName metadata
// record shaped like "total_bytes=2330599835" or "wal_bytes=1077881251", if
// present at this concurrency level. nil when absent or malformed, matching
// parseNetworkRTT's policy of never publishing a half-parsed value.
func sizeBytesInfo(records []metricRecord, recordName string) *int64 {
	for _, r := range records {
		if r.Name != recordName {
			continue
		}
		v, err := r.stringValue()
		if err != nil {
			continue
		}
		_, raw, ok := strings.Cut(v, "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			continue
		}
		return &n
	}
	return nil
}

// networkRTTInfo returns the driver-to-target round trip parsed from a
// network_rtt metadata record, if present at this concurrency level. A record
// that is absent or malformed yields nil: runs made before the probe existed
// have no such record, and one unreadable value shouldn't block publishing an
// otherwise good Result.
func networkRTTInfo(records []metricRecord) *networkInfo {
	for _, r := range records {
		if r.Name != "network_rtt" {
			continue
		}
		v, err := r.stringValue()
		if err != nil {
			continue
		}
		return parseNetworkRTT(v)
	}
	return nil
}

// parseNetworkRTT reads benchctl's "min_us=96, median_us=154, p99_us=299,
// max_us=337, samples=200" summary. It returns nil unless every field is
// present and numeric, so a partial parse never publishes a half-filled block.
func parseNetworkRTT(value string) *networkInfo {
	fields := map[string]int64{}
	for _, part := range strings.Split(value, ",") {
		key, raw, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			continue
		}
		fields[strings.TrimSpace(key)] = n
	}
	for _, key := range []string{"min_us", "median_us", "p99_us", "max_us", "samples"} {
		if _, ok := fields[key]; !ok {
			return nil
		}
	}
	return &networkInfo{
		RTTMinUs:    fields["min_us"],
		RTTMedianUs: fields["median_us"],
		RTTP99Us:    fields["p99_us"],
		RTTMaxUs:    fields["max_us"],
		Samples:     fields["samples"],
	}
}

// toolVersionInfo returns (benchctl_version, gotpc_version) from a
// concurrency level's records, if present. Unlike pg_version, benchctl
// stamps these onto every record as flat fields (like project_id/service),
// not as one dedicated metadata row, so any record carrying a value will do.
func toolVersionInfo(records []metricRecord) (benchctlVersion, gotpcVersion string) {
	for _, r := range records {
		if benchctlVersion == "" {
			benchctlVersion = r.BenchctlVersion
		}
		if gotpcVersion == "" {
			gotpcVersion = r.GotpcVersion
		}
	}
	return benchctlVersion, gotpcVersion
}

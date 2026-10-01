package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dbarena/dbarenactl/internal/pricing"
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
	Direction       string `json:"direction,omitempty"`
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

// stepWindow is when one benchctl step ran.
type stepWindow struct {
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
}

// loadStepWindows reads benchctl's step_windows.json in artifactDir and
// returns each concurrency level's measured "benchmark" step, keyed by its
// client_threads fixture value. It returns nil, not an error, when the file
// doesn't exist: runs made before benchctl wrote it have none.
func loadStepWindows(artifactDir string) (map[int]stepWindow, error) {
	path := filepath.Join(artifactDir, "step_windows.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var entries []struct {
		stepWindow
		Step    string            `json:"step"`
		Fixture map[string]string `json:"fixture"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	windows := map[int]stepWindow{}
	for _, e := range entries {
		if e.Step != "benchmark" {
			continue
		}
		threads, err := strconv.Atoi(e.Fixture["client_threads"])
		if err != nil {
			return nil, fmt.Errorf("%s: client_threads %q is not an integer: %w", path, e.Fixture["client_threads"], err)
		}
		windows[threads] = e.stepWindow
	}
	return windows, nil
}

// candidateRun is one successful, artifact-bearing run being considered as
// the representative iteration for a test point.
type candidateRun struct {
	run                 *sweepstate.Run
	metricsByThreads    map[int][]metricRecord
	rawSamplesByThreads map[int]string
	// stepWindows is nil for runs whose artifacts predate step_windows.json.
	stepWindows map[int]stepWindow
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

// selectRuns applies the documented reporting policy: at every concurrency
// level it picks the run with the median tpm, and the peak is the level whose
// median tpm is highest. Each reported figure still comes from exactly one
// run -- no pooling across a test point's independent iterations.
//
// Only levels every run has data for can be the peak, so a run missing a
// level can't decide it by omission. Ties go to the lower concurrency. For
// an even number of runs at a level, the lower median is chosen.
func selectRuns(runs []candidateRun) (peak int, median map[int]candidateRun, err error) {
	if len(runs) == 0 {
		return 0, nil, fmt.Errorf("no candidate runs to select from")
	}

	byThreads := map[int][]candidateRun{}
	for _, c := range runs {
		for threads := range c.metricsByThreads {
			byThreads[threads] = append(byThreads[threads], c)
		}
	}
	var levels []int
	for threads := range byThreads {
		levels = append(levels, threads)
	}
	sort.Ints(levels)

	median = map[int]candidateRun{}
	peak = -1
	var peakTpm float64
	for _, threads := range levels {
		type scored struct {
			c   candidateRun
			tpm float64
		}
		var scoredRuns []scored
		for _, c := range byThreads[threads] {
			tpm, err := tpmAt(c.metricsByThreads[threads])
			if err != nil {
				return 0, nil, fmt.Errorf("run %s at concurrency %d: %w", c.run.RunID, threads, err)
			}
			scoredRuns = append(scoredRuns, scored{c, tpm})
		}
		sort.SliceStable(scoredRuns, func(i, j int) bool { return scoredRuns[i].tpm < scoredRuns[j].tpm })
		m := scoredRuns[(len(scoredRuns)-1)/2]
		median[threads] = m.c

		if len(scoredRuns) == len(runs) && (peak == -1 || m.tpm > peakTpm) {
			peak, peakTpm = threads, m.tpm
		}
	}
	if peak == -1 {
		return 0, nil, fmt.Errorf("no concurrency level is common to all %d candidate runs", len(runs))
	}
	return peak, median, nil
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
			txns[name] = txnMetrics{Count: count, Tpm: pricing.RoundTo(tpm, 0), LatencyMs: lat}
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

// metadataFrom passes a concurrency level's metadata records through by
// name, so a new or changed benchctl record needs no change here. A metadata
// record is any record without a transaction: every go-tpc measurement
// carries one, metadata never does.
//
// A string value made only of "k=v" pairs separated by "," (e.g.
// "db_bytes=1,waldir_bytes=2") becomes an object; any other string is kept
// verbatim. A numeric value is nested under its direction and quantile
// labels, when present. Two records landing on the same path are an error
// rather than one silently overwriting the other.
func metadataFrom(records []metricRecord) (map[string]any, error) {
	metadata := map[string]any{}
	for _, r := range records {
		if r.Transaction != "" {
			continue
		}
		path := []string{r.Name}
		for _, label := range []string{r.Direction, r.Quantile} {
			if label != "" {
				path = append(path, label)
			}
		}
		var value any
		switch v := r.Value.(type) {
		case string:
			value = parseKeyValues(v)
		default:
			value = v
		}
		if err := setMetadataPath(metadata, path, value); err != nil {
			return nil, err
		}
	}
	return metadata, nil
}

// setMetadataPath stores value at path inside metadata, creating the
// intermediate objects. It fails if any part of path is already taken.
func setMetadataPath(metadata map[string]any, path []string, value any) error {
	node := metadata
	for i, key := range path[:len(path)-1] {
		existing, ok := node[key]
		if !ok {
			child := map[string]any{}
			node[key] = child
			node = child
			continue
		}
		child, ok := existing.(map[string]any)
		if !ok {
			return fmt.Errorf("metadata record %s: %s already holds a value", path[0], strings.Join(path[:i+1], "."))
		}
		node = child
	}
	last := path[len(path)-1]
	if _, ok := node[last]; ok {
		return fmt.Errorf("metadata record %s: duplicate value for %s", path[0], strings.Join(path, "."))
	}
	node[last] = value
	return nil
}

// parseKeyValues turns "k=v, k=v" into an object, keeping numeric values as
// numbers (verbatim, via json.Number, so e.g. "0.90" isn't reformatted). It
// returns value unchanged unless every comma-separated part is a k=v pair.
func parseKeyValues(value string) any {
	fields := map[string]any{}
	for _, part := range strings.Split(value, ",") {
		key, raw, ok := strings.Cut(strings.TrimSpace(part), "=")
		key, raw = strings.TrimSpace(key), strings.TrimSpace(raw)
		if !ok || key == "" {
			return value
		}
		if _, dup := fields[key]; dup {
			return value
		}
		if isJSONNumber(raw) {
			fields[key] = json.Number(raw)
		} else {
			fields[key] = raw
		}
	}
	return fields
}

func isJSONNumber(s string) bool {
	if s == "" || (s[0] != '-' && (s[0] < '0' || s[0] > '9')) {
		return false
	}
	return json.Valid([]byte(s))
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

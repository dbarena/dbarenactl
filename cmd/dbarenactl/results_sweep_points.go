package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/dbarena/dbarenactl/internal/pricing"
)

// sweepPointsResult is buildSweepPoints' return: the assembled sweep points
// plus the two facts (from pg_version) and the measured time span that only
// emerge while walking the selected run's records.
type sweepPointsResult struct {
	Points          []sweepPointJSON
	EngineVersion   string
	CPUArch         string
	BenchctlVersion string
	GotpcVersion    string
	PgSettings      string
	OrioleDBVersion string
	MeasuredFrom    time.Time
	MeasuredTo      time.Time
}

// sweepPointMeta bundles the facts buildSweepPoint gleans from a
// concurrency level's metadata records while assembling its sweepPointJSON,
// for buildSweepPoints to aggregate up into sweepPointsResult.
type sweepPointMeta struct {
	EngineVersion   string
	CPUArch         string
	BenchctlVersion string
	GotpcVersion    string
	PgSettings      string
	OrioleDBVersion string
}

// buildSweepPoints builds one sweepPointJSON per concurrency level the
// selected run has data for -- no other candidate contributes anything to
// the result itself, only to sweep[].iterations' reference listing (see
// buildIterationEntries).
func buildSweepPoints(scenario string, successful []candidateRun, selected candidateRun, warehouses *float64, scenarioDir string) (sweepPointsResult, error) {
	var threads []int
	for t := range selected.metricsByThreads {
		threads = append(threads, t)
	}
	sort.Ints(threads)
	var maxConcurrency int
	if len(threads) > 0 {
		maxConcurrency = threads[len(threads)-1]
	}

	result := sweepPointsResult{MeasuredFrom: selected.run.CreatedAt, MeasuredTo: selected.run.UpdatedAt}
	for _, c := range successful {
		if c.run.CreatedAt.Before(result.MeasuredFrom) {
			result.MeasuredFrom = c.run.CreatedAt
		}
		if c.run.UpdatedAt.After(result.MeasuredTo) {
			result.MeasuredTo = c.run.UpdatedAt
		}
	}

	for _, concurrency := range threads {
		point, meta, err := buildSweepPoint(scenario, concurrency, maxConcurrency, successful, selected, warehouses, scenarioDir)
		if err != nil {
			return sweepPointsResult{}, err
		}
		// Only overwrite when this level actually reports a value -- a
		// later level with no pg_version (or pg_settings/orioledb_version)
		// record shouldn't blank out an earlier level's answer.
		if meta.EngineVersion != "" {
			result.EngineVersion, result.CPUArch = meta.EngineVersion, meta.CPUArch
		}
		if meta.BenchctlVersion != "" {
			result.BenchctlVersion = meta.BenchctlVersion
		}
		if meta.GotpcVersion != "" {
			result.GotpcVersion = meta.GotpcVersion
		}
		if meta.PgSettings != "" {
			result.PgSettings = meta.PgSettings
		}
		if meta.OrioleDBVersion != "" {
			result.OrioleDBVersion = meta.OrioleDBVersion
		}
		result.Points = append(result.Points, point)
	}
	if len(result.Points) == 0 {
		return sweepPointsResult{}, fmt.Errorf("%s: selected run %s has no concurrency levels with data", scenario, selected.run.RunID)
	}
	return result, nil
}

// buildSweepPoint builds one concurrency level's sweepPointJSON from the
// selected run's own data, plus the reference listing of every other
// successful iteration's throughput at this concurrency (see
// buildIterationEntries).
func buildSweepPoint(scenario string, concurrency, maxConcurrency int, successful []candidateRun, selected candidateRun, warehouses *float64, scenarioDir string) (point sweepPointJSON, meta sweepPointMeta, err error) {
	records := selected.metricsByThreads[concurrency]
	tpm, err := tpmAt(records)
	if err != nil {
		return point, sweepPointMeta{}, fmt.Errorf("%s: concurrency %d: %w", scenario, concurrency, err)
	}
	p50, p95, p99, err := latencyFor(records, "NEW_ORDER")
	if err != nil {
		return point, sweepPointMeta{}, fmt.Errorf("%s: concurrency %d: %w", scenario, concurrency, err)
	}
	txns, errs, err := buildTxnMetrics(records)
	if err != nil {
		return point, sweepPointMeta{}, fmt.Errorf("%s: concurrency %d: %w", scenario, concurrency, err)
	}
	meta.EngineVersion, meta.CPUArch = pgVersionInfo(records)
	meta.BenchctlVersion, meta.GotpcVersion = toolVersionInfo(records)
	meta.PgSettings = pgSettingsInfo(records)
	meta.OrioleDBVersion = orioledbVersionInfo(records)

	var workloadParams map[string]any
	if warehouses != nil {
		workloadParams = map[string]any{"warehouses": *warehouses}
	}

	iterations, err := buildIterationEntries(scenario, concurrency, maxConcurrency, successful, selected, scenarioDir)
	if err != nil {
		return point, sweepPointMeta{}, err
	}

	point = sweepPointJSON{
		Concurrency:        concurrency,
		WorkloadParameters: workloadParams,
		Network:            networkRTTInfo(records),
		LoadDriver:         loadDriverInfoFrom(records),
		DBSizeBefore:       sizeBytesInfo(records, "db_size_before"),
		DBSizeAfter:        sizeBytesInfo(records, "db_size_after"),
		WALSizeBefore:      sizeBytesInfo(records, "wal_size_before"),
		WALSizeAfter:       sizeBytesInfo(records, "wal_size_after"),
		Iterations:         iterations,
		Summary: summaryInfo{
			Throughput: throughputInfo{Metric: "tpm", Unit: "transactions/min", Transaction: strPtr("NEW_ORDER"), Value: pricing.RoundTo(tpm, 0)},
			LatencyMs:  latencyInfo{Transaction: strPtr("NEW_ORDER"), P50: p50, P95: p95, P99: p99},
		},
		WorkloadMetrics: &workloadMetrics{Transactions: txns, Errors: errs},
	}
	return point, meta, nil
}

// buildIterationEntries lists every successful iteration's throughput at
// this concurrency -- not just the selected one -- so a reader can see the
// full spread, sorted by iteration number for a stable order. Only the
// selected iteration ever gets a raw-clients CSV written and referenced,
// and only at the sweep's maximum concurrency (maxConcurrency): no
// pooling/intermingling across a test point's independent iterations
// (selectRepresentativeRun's policy), and one raw-clients CSV per sweep
// rather than one per concurrency level.
func buildIterationEntries(scenario string, concurrency, maxConcurrency int, successful []candidateRun, selected candidateRun, scenarioDir string) ([]iterationEntry, error) {
	var iterations []iterationEntry
	for _, c := range successful {
		candidateRecords, ok := c.metricsByThreads[concurrency]
		if !ok {
			continue
		}
		candidateTpm, err := tpmAt(candidateRecords)
		if err != nil {
			return nil, fmt.Errorf("%s: concurrency %d: run %s: %w", scenario, concurrency, c.run.RunID, err)
		}

		var notes *string
		var rawMetricsFile *string
		if c.run.RunID == selected.run.RunID {
			notes = strPtr("This iteration's data (median tpm at this test point's peak concurrency) is used " +
				"for summary/workload_metrics below.")
			if concurrency == maxConcurrency {
				if rawPath, ok := c.rawSamplesByThreads[concurrency]; ok {
					relName := fmt.Sprintf("raw-clients-%d.csv", concurrency)
					if err := writeRawClientsCSV(rawPath, filepath.Join(scenarioDir, relName)); err != nil {
						fmt.Fprintf(os.Stderr, "warning: %s: concurrency %d: could not write raw-clients csv: %v\n", scenario, concurrency, err)
					} else {
						rawMetricsFile = &relName
					}
				}
			}
		} else {
			notes = strPtr("started_at/completed_at reflect the whole run's span, not this specific concurrency.")
		}
		iterations = append(iterations, iterationEntry{
			Iteration:      c.run.IterationAttempt,
			StartedAt:      c.run.CreatedAt.UTC().Format(time.RFC3339),
			CompletedAt:    c.run.UpdatedAt.UTC().Format(time.RFC3339),
			Throughput:     pricing.RoundTo(candidateTpm, 0),
			RawMetricsFile: rawMetricsFile,
			Notes:          notes,
		})
	}
	sort.Slice(iterations, func(i, j int) bool { return iterations[i].Iteration < iterations[j].Iteration })
	return iterations, nil
}

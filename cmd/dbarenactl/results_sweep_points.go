package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
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
	MeasuredFrom    time.Time
	MeasuredTo      time.Time
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
		point, engineVersion, cpuArch, benchctlVersion, gotpcVersion, err := buildSweepPoint(scenario, concurrency, successful, selected, warehouses, scenarioDir)
		if err != nil {
			return sweepPointsResult{}, err
		}
		// Only overwrite when this level actually reports a version -- a
		// later level with no pg_version record shouldn't blank out an
		// earlier level's answer.
		if engineVersion != "" {
			result.EngineVersion, result.CPUArch = engineVersion, cpuArch
		}
		if benchctlVersion != "" {
			result.BenchctlVersion = benchctlVersion
		}
		if gotpcVersion != "" {
			result.GotpcVersion = gotpcVersion
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
func buildSweepPoint(scenario string, concurrency int, successful []candidateRun, selected candidateRun, warehouses *float64, scenarioDir string) (point sweepPointJSON, engineVersion, cpuArch, benchctlVersion, gotpcVersion string, err error) {
	records := selected.metricsByThreads[concurrency]
	tpm, err := tpmAt(records)
	if err != nil {
		return point, "", "", "", "", fmt.Errorf("%s: concurrency %d: %w", scenario, concurrency, err)
	}
	p50, p95, p99, err := latencyFor(records, "NEW_ORDER")
	if err != nil {
		return point, "", "", "", "", fmt.Errorf("%s: concurrency %d: %w", scenario, concurrency, err)
	}
	txns, errs, err := buildTxnMetrics(records)
	if err != nil {
		return point, "", "", "", "", fmt.Errorf("%s: concurrency %d: %w", scenario, concurrency, err)
	}
	engineVersion, cpuArch = pgVersionInfo(records)
	benchctlVersion, gotpcVersion = toolVersionInfo(records)

	var workloadParams map[string]any
	if warehouses != nil {
		workloadParams = map[string]any{"warehouses": *warehouses}
	}

	iterations, err := buildIterationEntries(scenario, concurrency, successful, selected, scenarioDir)
	if err != nil {
		return point, "", "", "", "", err
	}

	point = sweepPointJSON{
		Concurrency:        concurrency,
		WorkloadParameters: workloadParams,
		Network:            networkRTTInfo(records),
		Iterations:         iterations,
		Summary: summaryInfo{
			Throughput: throughputInfo{Metric: "tpm", Unit: "transactions/min", Transaction: strPtr("NEW_ORDER"), Value: tpm},
			LatencyMs:  latencyInfo{Transaction: strPtr("NEW_ORDER"), P50: p50, P95: p95, P99: p99},
		},
		WorkloadMetrics: &workloadMetrics{Transactions: txns, Errors: errs},
	}
	return point, engineVersion, cpuArch, benchctlVersion, gotpcVersion, nil
}

// buildIterationEntries lists every successful iteration's throughput at
// this concurrency -- not just the selected one -- so a reader can see the
// full spread, sorted by iteration number for a stable order. Only the
// selected iteration ever gets a raw-clients CSV written and referenced:
// no pooling/intermingling across a test point's independent iterations
// (selectRepresentativeRun's policy).
func buildIterationEntries(scenario string, concurrency int, successful []candidateRun, selected candidateRun, scenarioDir string) ([]iterationEntry, error) {
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
			if rawPath, ok := c.rawSamplesByThreads[concurrency]; ok {
				relName := fmt.Sprintf("raw-clients-%d.csv", concurrency)
				if err := writeRawClientsCSV(rawPath, filepath.Join(scenarioDir, relName)); err != nil {
					fmt.Fprintf(os.Stderr, "warning: %s: concurrency %d: could not write raw-clients csv: %v\n", scenario, concurrency, err)
				} else {
					rawMetricsFile = &relName
				}
			}
		} else {
			notes = strPtr("started_at/completed_at reflect the whole run's span, not this specific concurrency.")
		}
		iterations = append(iterations, iterationEntry{
			Iteration:      c.run.IterationAttempt,
			StartedAt:      c.run.CreatedAt.UTC().Format(time.RFC3339),
			CompletedAt:    c.run.UpdatedAt.UTC().Format(time.RFC3339),
			Throughput:     candidateTpm,
			RawMetricsFile: rawMetricsFile,
			Notes:          notes,
		})
	}
	sort.Slice(iterations, func(i, j int) bool { return iterations[i].Iteration < iterations[j].Iteration })
	return iterations, nil
}

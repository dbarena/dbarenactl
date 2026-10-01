package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// sweepPointsResult is buildSweepPoints' return: the assembled sweep points
// plus the test-point facts and the measured time span that only emerge
// while walking the runs' records.
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

// sweepPointMeta bundles the test-point facts one concurrency level's
// records carry, for buildSweepPoints to aggregate up into sweepPointsResult.
type sweepPointMeta struct {
	EngineVersion   string
	CPUArch         string
	BenchctlVersion string
	GotpcVersion    string
	PgSettings      string
	OrioleDBVersion string
}

// buildSweepPoints builds one sweepPointJSON per concurrency level any run
// has data for, listing every run's measurement at that level. median and
// peak come from selectRuns. Facts about the test point itself (versions,
// settings) are read from the median runs' records.
func buildSweepPoints(scenario string, runs []candidateRun, peak int, median map[int]candidateRun, warehouses *float64, scenarioDir string) (sweepPointsResult, error) {
	var levels []int
	for threads := range median {
		levels = append(levels, threads)
	}
	sort.Ints(levels)

	var result sweepPointsResult
	for i, c := range runs {
		if i == 0 || c.run.CreatedAt.Before(result.MeasuredFrom) {
			result.MeasuredFrom = c.run.CreatedAt
		}
		if i == 0 || c.run.UpdatedAt.After(result.MeasuredTo) {
			result.MeasuredTo = c.run.UpdatedAt
		}
	}

	var workloadParams map[string]any
	if warehouses != nil {
		workloadParams = map[string]any{"warehouses": *warehouses}
	}

	for _, concurrency := range levels {
		medianRun := median[concurrency]
		meta := sweepPointMetaFrom(medianRun.metricsByThreads[concurrency])
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

		var iterations []iterationEntry
		for _, c := range runs {
			if _, ok := c.metricsByThreads[concurrency]; !ok {
				continue
			}
			it, err := buildIteration(c, concurrency, c.run.RunID == medianRun.run.RunID, scenarioDir)
			if err != nil {
				return sweepPointsResult{}, fmt.Errorf("%s: concurrency %d: run %s: %w", scenario, concurrency, c.run.RunID, err)
			}
			iterations = append(iterations, it)
		}
		sort.Slice(iterations, func(i, j int) bool { return iterations[i].Iteration < iterations[j].Iteration })

		result.Points = append(result.Points, sweepPointJSON{
			Concurrency:        concurrency,
			Peak:               concurrency == peak,
			WorkloadParameters: workloadParams,
			Iterations:         iterations,
		})
	}
	if len(result.Points) == 0 {
		return sweepPointsResult{}, fmt.Errorf("%s: no concurrency levels with data", scenario)
	}
	return result, nil
}

// sweepPointMetaFrom reads the test-point facts one concurrency level's
// records carry.
func sweepPointMetaFrom(records []metricRecord) sweepPointMeta {
	var meta sweepPointMeta
	meta.EngineVersion, meta.CPUArch = pgVersionInfo(records)
	meta.BenchctlVersion, meta.GotpcVersion = toolVersionInfo(records)
	meta.PgSettings = pgSettingsInfo(records)
	meta.OrioleDBVersion = orioledbVersionInfo(records)
	return meta
}

// buildIteration builds one run's measurement at one concurrency level and
// writes that run's raw-clients CSV next to result.json.
func buildIteration(c candidateRun, concurrency int, isMedian bool, scenarioDir string) (iterationEntry, error) {
	records := c.metricsByThreads[concurrency]
	txns, errs, err := buildTxnMetrics(records)
	if err != nil {
		return iterationEntry{}, err
	}
	metadata, err := metadataFrom(records)
	if err != nil {
		return iterationEntry{}, err
	}

	// started_at/completed_at are the measured benchmark step at this
	// concurrency level.
	// TODO: remove the fallback to the run's span once all runs carry step_windows.json.
	startedAt, completedAt := c.run.CreatedAt, c.run.UpdatedAt
	if w, ok := c.stepWindows[concurrency]; ok {
		startedAt, completedAt = w.StartedAt, w.EndedAt
	}

	var rawMetricsFile *string
	if rawPath, ok := c.rawSamplesByThreads[concurrency]; ok {
		relName := fmt.Sprintf("raw-clients-%d-iteration-%d.csv", concurrency, c.run.IterationAttempt)
		if err := writeRawClientsCSV(rawPath, filepath.Join(scenarioDir, relName)); err != nil {
			fmt.Fprintf(os.Stderr, "warning: concurrency %d: run %s: could not write raw-clients csv: %v\n", concurrency, c.run.RunID, err)
		} else {
			rawMetricsFile = &relName
		}
	}

	return iterationEntry{
		Iteration:       c.run.IterationAttempt,
		Median:          isMedian,
		StartedAt:       startedAt.UTC().Format(time.RFC3339),
		CompletedAt:     completedAt.UTC().Format(time.RFC3339),
		WorkloadMetrics: &workloadMetrics{Transactions: txns, Errors: errs},
		Metadata:        metadata,
		RawMetricsFile:  rawMetricsFile,
	}, nil
}

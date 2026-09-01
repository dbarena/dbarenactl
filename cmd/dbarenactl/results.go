package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/spf13/cobra"

	"github.com/dbarena/dbarenactl/internal/manifest"
	"github.com/dbarena/dbarenactl/internal/pricing"
	"github.com/dbarena/dbarenactl/internal/sweepstate"
)

var (
	resultsDest      string
	resultsCandidate string
	resultsForce     bool
)

var resultsCmd = &cobra.Command{
	Use:   "results <sweep-id>",
	Short: "Assemble result.json files from a sweep's fetched artifacts, ready to submit to dbarena/dbarena",
	Args:  cobra.ExactArgs(1),
	RunE:  runResults,
}

func init() {
	resultsCmd.Flags().StringVar(&resultsDest, "dest", "",
		"Directory to write results/ into. Defaults to the current directory if it's a dbarena checkout, "+
			"or a sibling ../dbarena directory if that's one instead; otherwise a local scratch directory")
	resultsCmd.Flags().StringVar(&resultsCandidate, "candidate", "",
		"Path to the candidate manifest to use, overriding the one recorded when the sweep was created "+
			"(needed if that recorded path can't be resolved from the current directory)")
	resultsCmd.Flags().BoolVar(&resultsForce, "force", false,
		"Emit a result even for test points that didn't reach their required number of successful iterations")
}

func runResults(_ *cobra.Command, args []string) error {
	sweepID := args[0]

	dbFile, err := dbPath()
	if err != nil {
		return err
	}
	store, err := sweepstate.Open(dbFile)
	if err != nil {
		return err
	}
	defer store.Close() //nolint:errcheck

	sweep, err := store.GetSweep(sweepID)
	if err != nil {
		return fmt.Errorf("dbarenactl results: sweep %s: %w", sweepID, err)
	}
	var params sweepParams
	if err := json.Unmarshal([]byte(sweep.ParamsJSON), &params); err != nil {
		return fmt.Errorf("dbarenactl results: sweep %s: corrupt params: %w", sweepID, err)
	}

	manifestPath := resultsCandidate
	if manifestPath == "" {
		manifestPath = params.ManifestPath
	}
	m, err := manifest.Load(manifestPath)
	if err != nil {
		return fmt.Errorf(
			"dbarenactl results: load candidate manifest %q: %w -- if this sweep's recorded manifest path "+
				"no longer resolves from here, pass --candidate explicitly", manifestPath, err)
	}

	pricingStore, err := openPricingStore()
	if err != nil {
		return err
	}
	defer pricingStore.Close() //nolint:errcheck

	testPoints, err := store.ListTestPoints(sweepID)
	if err != nil {
		return err
	}
	if len(testPoints) == 0 {
		return fmt.Errorf("dbarenactl results: sweep %s has no test points", sweepID)
	}

	dest, isCheckout, err := resolveDest(resultsDest, sweepID)
	if err != nil {
		return err
	}
	var schema *jsonschema.Schema
	if isCheckout {
		schema, err = compileResultSchema(dest)
		if err != nil {
			return fmt.Errorf("dbarenactl results: compile %s/results/schema/result.schema.json: %w", dest, err)
		}
	} else {
		fmt.Fprintf(os.Stderr,
			"note: %s doesn't look like a dbarena checkout (no results/schema/result.schema.json) -- "+
				"writing plain files there with no schema validation and no index.json update. "+
				"Copy them into a dbarena checkout's results/ directory when ready.\n", dest)
	}

	snapshot, snapErr := pricingStore.GetLatestSnapshot(m.Provider, m.Product, m.Plan, m.Region)
	if snapErr != nil && !errors.Is(snapErr, pricing.ErrNotFound) {
		return fmt.Errorf("dbarenactl results: look up pricing snapshot: %w", snapErr)
	}
	if errors.Is(snapErr, pricing.ErrNotFound) {
		fmt.Fprintf(os.Stderr,
			"warning: no pricing snapshot found for %s/%s/%s/%s -- results will be written with pricing: null. "+
				"Run `dbarenactl pricing fetch --candidate %s` (or `pricing set`) first to include cost data.\n",
			m.Provider, m.Product, m.Plan, m.Region, manifestPath)
	}

	var written, skipped []string
	for _, tp := range testPoints {
		scenario := scenarioSlug(tp.BoundType, tp.Tier, tp.Variant)
		def := findTestPointDef(m, tp)
		if def == nil {
			fmt.Fprintf(os.Stderr, "warning: skipping %s: no matching test point in the current manifest (tier=%s bound_type=%s variant=%s)\n",
				scenario, tp.Tier, tp.BoundType, tp.Variant)
			skipped = append(skipped, scenario)
			continue
		}

		runs, err := store.ListRunsForTestPoint(tp.ID)
		if err != nil {
			return err
		}
		var successful []candidateRun
		for _, r := range runs {
			if r.Outcome != "success" || r.LocalArtifactDir == "" {
				continue
			}
			metrics, err := loadRunMetrics(r.LocalArtifactDir)
			if err != nil {
				return fmt.Errorf("dbarenactl results: %s: read artifacts for run %s: %w", scenario, r.RunID, err)
			}
			if len(metrics) == 0 {
				continue
			}
			successful = append(successful, candidateRun{run: r, metricsByThreads: metrics})
		}

		if len(successful) < tp.SuccessesNeeded && !resultsForce {
			fmt.Fprintf(os.Stderr, "warning: skipping %s: only %d/%d successful iterations (use --force to emit anyway)\n",
				scenario, len(successful), tp.SuccessesNeeded)
			skipped = append(skipped, scenario)
			continue
		}
		if len(successful) == 0 {
			fmt.Fprintf(os.Stderr, "warning: skipping %s: no successful iterations with fetched artifacts\n", scenario)
			skipped = append(skipped, scenario)
			continue
		}

		doc, err := buildResultDoc(m, tp, def, successful, snapshot, manifestPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping %s: %v\n", scenario, err)
			skipped = append(skipped, scenario)
			continue
		}
		if len(successful) < tp.SuccessesNeeded {
			doc.Reproducibility.Notes = strPtr(fmt.Sprintf(
				"Only %d/%d configured iterations succeeded; --force was used to emit this result anyway.",
				len(successful), tp.SuccessesNeeded))
		}

		provider, err := providerSlug(m.Provider)
		if err != nil {
			return err
		}
		data, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return fmt.Errorf("dbarenactl results: %s: marshal result.json: %w", scenario, err)
		}
		if schema != nil {
			if err := validateAgainstSchema(schema, scenario, data); err != nil {
				return fmt.Errorf("dbarenactl results: %w", err)
			}
		}

		scenarioDir := filepath.Join(dest, "results", provider, m.Workload, scenario)
		if err := os.MkdirAll(scenarioDir, 0o755); err != nil {
			return fmt.Errorf("dbarenactl results: %s: %w", scenario, err)
		}
		if err := os.WriteFile(filepath.Join(scenarioDir, "result.json"), append(data, '\n'), 0o644); err != nil {
			return fmt.Errorf("dbarenactl results: %s: %w", scenario, err)
		}
		if isCheckout {
			relPath := filepath.Join(provider, m.Workload, scenario, "result.json")
			if err := updateResultsIndex(dest, provider, m.Workload, scenario, relPath); err != nil {
				return fmt.Errorf("dbarenactl results: %s: update index.json: %w", scenario, err)
			}
		}
		written = append(written, scenario)
	}

	fmt.Printf("\n%d result(s) written to %s, %d skipped.\n", len(written), filepath.Join(dest, "results"), len(skipped))
	if len(skipped) > 0 {
		sort.Strings(skipped)
		fmt.Printf("skipped: %v\n", skipped)
	}
	return nil
}

// findTestPointDef locates the manifest test point definition matching a
// sweepstate TestPoint's identity (tier/bound_type/variant) -- deliberately
// read from the freshly loaded manifest, not the sweep's frozen set_json,
// since the manifest is the durable, correctable source of truth for static
// sizing facts and may have been edited since the sweep ran.
func findTestPointDef(m *manifest.Manifest, tp *sweepstate.TestPoint) *manifest.TestPointDef {
	d, ok := m.FindTestPoint(tp.Tier, tp.BoundType, tp.Variant)
	if !ok {
		return nil
	}
	return d
}

// setFloat parses a numeric set: value, returning nil if the key is absent
// or unparsable (schema fields fed from this are all nullable).
func setFloat(set map[string]string, key string) *float64 {
	raw, ok := set[key]
	if !ok {
		return nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil
	}
	return &v
}

func buildResultDoc(m *manifest.Manifest, tp *sweepstate.TestPoint, def *manifest.TestPointDef, successful []candidateRun, snapshot *pricing.Snapshot, manifestPath string) (*resultDoc, error) {
	selected, peak, err := selectRepresentativeRun(successful)
	if err != nil {
		return nil, err
	}
	_ = peak

	provider, err := providerSlug(m.Provider)
	if err != nil {
		return nil, err
	}
	scenario := scenarioSlug(tp.BoundType, tp.Tier, tp.Variant)

	instanceType := def.Set["db_instance_type"]
	if instanceType == "" {
		instanceType = def.Set["project_size"]
	}
	diskType := def.Set["disk_type"]
	diskGB := setFloat(def.Set, "disk_size_gb")
	iops := setFloat(def.Set, "disk_iops")
	throughputMbps := setFloat(def.Set, "disk_throughput_mibps")
	diskBaselineIOPS := setFloat(def.Set, "disk_baseline_iops")
	diskBaselineThroughput := setFloat(def.Set, "disk_baseline_throughput_mibps")

	// Build sweep points from the selected run's own data only, across
	// every concurrency level it has data for -- no other iteration
	// contributes anything to this result.
	var threads []int
	for t := range selected.metricsByThreads {
		threads = append(threads, t)
	}
	sort.Ints(threads)

	warehouses := setFloat(def.Set, "warehouses")

	var sweepPoints []sweepPointJSON
	var engineVersion, cpuArch string
	measuredFrom, measuredTo := selected.run.CreatedAt, selected.run.UpdatedAt
	for _, c := range successful {
		if c.run.CreatedAt.Before(measuredFrom) {
			measuredFrom = c.run.CreatedAt
		}
		if c.run.UpdatedAt.After(measuredTo) {
			measuredTo = c.run.UpdatedAt
		}
	}

	for _, concurrency := range threads {
		records := selected.metricsByThreads[concurrency]
		tpm, err := tpmCAt(records)
		if err != nil {
			return nil, fmt.Errorf("%s: concurrency %d: %w", scenario, concurrency, err)
		}
		p50, p95, p99, err := latencyFor(records, "NEW_ORDER")
		if err != nil {
			return nil, fmt.Errorf("%s: concurrency %d: %w", scenario, concurrency, err)
		}
		txns, errs, err := buildTxnMetrics(records)
		if err != nil {
			return nil, fmt.Errorf("%s: concurrency %d: %w", scenario, concurrency, err)
		}
		if v, arch := pgVersionInfo(records); v != "" {
			engineVersion, cpuArch = v, arch
		}

		var workloadParams map[string]any
		if warehouses != nil {
			workloadParams = map[string]any{"warehouses": *warehouses}
		}

		// Every successful iteration with data at this concurrency is
		// listed -- not just the one used for summary/workload_metrics
		// below -- so a reader can see the full spread. Sorted by
		// iteration number for a stable, readable order.
		var iterations []iterationEntry
		for _, c := range successful {
			candidateRecords, ok := c.metricsByThreads[concurrency]
			if !ok {
				continue
			}
			candidateTpm, err := tpmCAt(candidateRecords)
			if err != nil {
				return nil, fmt.Errorf("%s: concurrency %d: run %s: %w", scenario, concurrency, c.run.RunID, err)
			}
			var notes *string
			if c.run.RunID == selected.run.RunID {
				notes = strPtr("This iteration's data (median tpmC at this test point's peak concurrency) is used " +
					"for summary/workload_metrics below; the other iterations are listed for reference only. " +
					"started_at/completed_at reflect the whole run's span, not this specific concurrency point -- " +
					"dbarenactl does not currently capture per-fixture timestamps.")
			} else {
				notes = strPtr("started_at/completed_at reflect the whole run's span, not this specific " +
					"concurrency point -- dbarenactl does not currently capture per-fixture timestamps.")
			}
			iterations = append(iterations, iterationEntry{
				Iteration:      c.run.IterationAttempt,
				StartedAt:      c.run.CreatedAt.UTC().Format(time.RFC3339),
				CompletedAt:    c.run.UpdatedAt.UTC().Format(time.RFC3339),
				Throughput:     candidateTpm,
				RawMetricsFile: nil,
				Notes:          notes,
			})
		}
		sort.Slice(iterations, func(i, j int) bool { return iterations[i].Iteration < iterations[j].Iteration })

		sweepPoints = append(sweepPoints, sweepPointJSON{
			Concurrency:        concurrency,
			WorkloadParameters: workloadParams,
			Iterations:         iterations,
			Summary: summaryInfo{
				Throughput: throughputInfo{Metric: "tpmC", Unit: "transactions/min", Transaction: strPtr("NEW_ORDER"), Value: tpm},
				LatencyMs:  latencyInfo{Transaction: strPtr("NEW_ORDER"), P50: p50, P95: p95, P99: p99},
			},
			WorkloadMetrics: &workloadMetrics{Transactions: txns, Errors: errs},
		})
	}
	if len(sweepPoints) == 0 {
		return nil, fmt.Errorf("%s: selected run %s has no concurrency levels with data", scenario, selected.run.RunID)
	}

	// vcpu/ram_gb, when a pricing snapshot is available to derive them from.
	var vcpu, ramGB *float64
	if snapshot != nil && instanceType != "" {
		if fn, ok := newVCPURAMFuncs()[m.PricingFetcherKey()]; ok {
			if v, r, err := fn(snapshot.Items, instanceType); err == nil {
				vcpu, ramGB = &v, &r
			}
		}
	}

	instance := &instanceInfo{
		InstanceType:   strPtr(instanceType),
		VCPU:           vcpu,
		RAMGB:          ramGB,
		DiskGB:         diskGB,
		IOPS:           iops,
		ThroughputMbps: throughputMbps,
		CPUArch:        strPtr(cpuArch),
		EngineVersion:  strPtr(engineVersion),
	}

	// reproducibility.command is the dbarenactl-level invocation that
	// reproduces this exact test point -- not a raw benchctl command. That
	// sidesteps ever needing to redact a run-supplied value (e.g.
	// supabase_org_id) from it, since no --set list is shown at all: every
	// value that would have appeared there (warehouses, disk sizing, ...) is
	// already reported in `instance`/`sweep[].workload_parameters` above.
	absManifestPath, err := filepath.Abs(manifestPath)
	if err != nil {
		absManifestPath = manifestPath
	}
	testPointKey := manifest.TestPointDef{Tier: tp.Tier, BoundType: tp.BoundType, Variant: tp.Variant}.Key()
	command := fmt.Sprintf("dbarenactl run --candidate %s --test-point %s", repoRelativePath(absManifestPath), testPointKey)
	repro := reproducibility{
		BenchctlVersion: nil,
		LoadGenerator:   loadGeneratorInfo{Name: strPtr("https://github.com/supabase/go-tpc/"), Version: nil},
		ScenarioRepo:    scenarioRepoInfo{URL: nil, Commit: nil},
		ScenarioPath:    strPtr(repoRelativePath(m.ResolvedScenarioPath())),
		Command:         strPtr(command),
		Notes: strPtr("command is the dbarenactl invocation that reproduces this exact test point, not a raw " +
			"benchctl command -- it omits --set overrides, which are already captured in instance/workload_parameters " +
			"above. benchctl_version, load_generator.version, and scenario_repo are not currently captured by " +
			"dbarenactl or benchctl."),
	}

	var pricingOut *pricingInfo
	if snapshot != nil {
		calc, ok := newCostCalculators()[m.PricingFetcherKey()]
		if !ok {
			return nil, fmt.Errorf("%s: no cost calculator registered for %q", scenario, m.PricingFetcherKey())
		}
		costInput := pricing.CostInput{
			InstanceType: instanceType, DiskType: diskType,
			DiskBaselineIOPS: diskBaselineIOPS, DiskBaselineThroughputMbps: diskBaselineThroughput,
		}
		if diskGB != nil {
			costInput.DiskGB = *diskGB
		}
		if iops != nil {
			costInput.IOPS = *iops
		}
		if throughputMbps != nil {
			costInput.ThroughputMbps = *throughputMbps
		}
		breakdown, err := calc.Cost(snapshot.Items, costInput)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s: could not compute pricing: %v -- writing pricing: null\n", scenario, err)
		} else {
			if err := appendPricingAuditLog(tp.SweepID, scenario, costInput, snapshot, breakdown); err != nil {
				fmt.Fprintf(os.Stderr, "warning: %s: could not write pricing audit log: %v\n", scenario, err)
			}
			pricingOut = &pricingInfo{
				MonthlyUSD: breakdown.TotalUSD, HoursPerMonth: pricing.HoursPerMonth,
				PricingModel: "on-demand-list-price",
				Source:       strPtr(fmt.Sprintf("dbarenactl pricing snapshot %s, fetched %s", snapshot.ID, snapshot.FetchedAt.UTC().Format(time.RFC3339))),
			}
			for i := range sweepPoints {
				v := sweepPoints[i].Summary.Throughput.Value / pricingOut.MonthlyUSD
				sweepPoints[i].Summary.TpmcPerDollarMonth = &v
			}
		}
	}

	return &resultDoc{
		SchemaVersion:   "1.0.0",
		Provider:        provider,
		Workload:        m.Workload,
		Scenario:        scenario,
		Tier:            strPtr(tp.Tier),
		BoundType:       tp.BoundType,
		Variant:         strPtr(tp.Variant),
		Instance:        instance,
		MeasuredFrom:    measuredFrom.UTC().Format(time.RFC3339),
		MeasuredTo:      measuredTo.UTC().Format(time.RFC3339),
		Reproducibility: repro,
		Pricing:         pricingOut,
		Sweep:           sweepPoints,
	}, nil
}

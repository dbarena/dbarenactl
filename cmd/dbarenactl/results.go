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
			metrics, rawSamples, err := loadRunMetrics(r.LocalArtifactDir)
			if err != nil {
				return fmt.Errorf("dbarenactl results: %s: read artifacts for run %s: %w", scenario, r.RunID, err)
			}
			if len(metrics) == 0 {
				continue
			}
			successful = append(successful, candidateRun{run: r, metricsByThreads: metrics, rawSamplesByThreads: rawSamples})
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

		provider := slugify(m.Provider)
		product := slugify(m.Product)
		// Computed before buildResultDoc (not after, as result.json's own
		// write used to be) so buildResultDoc can write raw-clients-<n>.csv
		// into it directly, alongside setting the matching iteration's
		// raw_metrics_file -- doc and CSV need to agree with each other,
		// and buildResultDoc is the one place that already knows which
		// iteration was selected.
		scenarioDir := filepath.Join(dest, "results", provider, product, m.Workload, scenario)
		if err := os.MkdirAll(scenarioDir, 0o755); err != nil {
			return fmt.Errorf("dbarenactl results: %s: %w", scenario, err)
		}

		doc, err := buildResultDoc(resultDocInputs{
			Manifest:     m,
			TestPoint:    tp,
			Def:          def,
			Successful:   successful,
			Snapshot:     snapshot,
			ManifestPath: manifestPath,
			ScenarioDir:  scenarioDir,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping %s: %v\n", scenario, err)
			skipped = append(skipped, scenario)
			continue
		}
		if len(successful) < tp.SuccessesNeeded {
			fmt.Fprintf(os.Stderr, "warning: %s: only %d/%d configured iterations succeeded; emitting anyway (--force)\n",
				scenario, len(successful), tp.SuccessesNeeded)
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

		if err := os.WriteFile(filepath.Join(scenarioDir, "result.json"), append(data, '\n'), 0o644); err != nil {
			return fmt.Errorf("dbarenactl results: %s: %w", scenario, err)
		}
		if isCheckout {
			relPath := filepath.Join(provider, product, m.Workload, scenario, "result.json")
			if err := updateResultsIndex(dest, provider, product, m.Workload, scenario, tp.Variant, relPath); err != nil {
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

// pricingInputs is the set of sizing/pricing facts buildResultDoc needs for
// one test point, split by where they're sourced from: Set fields are real
// benchctl inputs (also needed for pricing), Pricing fields are
// dbarenactl-only metadata that no benchctl scenario declares as an input
// (see TestPointDef.Pricing's doc comment) and are read only here.
type pricingInputs struct {
	instanceType           string
	diskType               string
	diskGB                 *float64
	iops                   *float64
	throughputMbps         *float64
	dataCacheGB            *float64
	warehouses             *float64
	diskBaselineIOPS       *float64
	diskBaselineThroughput *float64
}

// resolvePricingInputs reads pricingInputs from a test point definition.
// instanceType's source depends on which provider owns def, since each names
// its compute SKU differently, but all three now declare it as a real
// benchctl scenario input, read from Set: AWS's db_instance_class, GCP's
// db_instance_type, and Supabase's project_size (which doubles as its own
// compute SKU). diskType is similar for GCP (a real benchctl input,
// disk_type, also read from Set) but not for AWS/Supabase, whose disk type
// (gp3) isn't a benchctl input -- it's stated explicitly under Pricing.disk_type
// instead.
func resolvePricingInputs(provider string, def *manifest.TestPointDef) pricingInputs {
	var instanceType, diskType string
	switch provider {
	case manifest.ProviderAWS:
		instanceType = def.Set["db_instance_class"]
		diskType = def.Pricing["disk_type"]
	case manifest.ProviderGCP:
		instanceType = def.Set["db_instance_type"]
		diskType = def.Set["disk_type"]
	case manifest.ProviderSupabase:
		instanceType = def.Set["project_size"]
		diskType = def.Pricing["disk_type"]
	}
	return pricingInputs{
		instanceType:           instanceType,
		diskType:               diskType,
		diskGB:                 setFloat(def.Set, "disk_size_gb"),
		iops:                   setFloat(def.Set, "disk_iops"),
		throughputMbps:         setFloat(def.Set, "disk_throughput_mibps"),
		dataCacheGB:            setFloat(def.Pricing, "data_cache_gb"),
		warehouses:             setFloat(def.Set, "warehouses"),
		diskBaselineIOPS:       setFloat(def.Pricing, "disk_baseline_iops"),
		diskBaselineThroughput: setFloat(def.Pricing, "disk_baseline_throughput_mibps"),
	}
}

// resultDocInputs bundles buildResultDoc's inputs. A plain struct instead
// of positional params mainly because manifestPath and scenarioDir are two
// adjacent, easily-swapped strings with very different meanings, and
// because every one of these already has to converge on the orchestrator
// regardless of how its body is split into sub-functions.
type resultDocInputs struct {
	Manifest     *manifest.Manifest
	TestPoint    *sweepstate.TestPoint
	Def          *manifest.TestPointDef
	Successful   []candidateRun
	Snapshot     *pricing.Snapshot
	ManifestPath string
	ScenarioDir  string
}

// buildResultDoc assembles one test point's result.json. It's a thin
// orchestrator over four independent pieces -- buildSweepPoints,
// buildInstanceInfo, buildReproducibility, computePricing -- each of which
// takes only what it needs, not this function's full input set.
func buildResultDoc(in resultDocInputs) (*resultDoc, error) {
	selected, _, err := selectRepresentativeRun(in.Successful)
	if err != nil {
		return nil, err
	}

	provider := slugify(in.Manifest.Provider)
	product := slugify(in.Manifest.Product)
	scenario := scenarioSlug(in.TestPoint.BoundType, in.TestPoint.Tier, in.TestPoint.Variant)
	pi := resolvePricingInputs(in.Manifest.Provider, in.Def)

	sp, err := buildSweepPoints(scenario, in.Successful, selected, pi.warehouses, in.ScenarioDir)
	if err != nil {
		return nil, err
	}

	pricingFetcherKey := in.Manifest.PricingFetcherKey()
	instance := buildInstanceInfo(in.Snapshot, pricingFetcherKey, pi, sp.CPUArch, sp.EngineVersion)
	repro := buildReproducibility(in.TestPoint, in.ManifestPath, sp.BenchctlVersion, sp.GotpcVersion)

	pricingOut, err := computePricing(in.Snapshot, pricingFetcherKey, pi, scenario, sp.Points)
	if err != nil {
		return nil, err
	}

	return &resultDoc{
		SchemaVersion:   "1.3.0",
		Provider:        provider,
		Product:         product,
		Workload:        in.Manifest.Workload,
		Scenario:        scenario,
		Tier:            strPtr(in.TestPoint.Tier),
		BoundType:       in.TestPoint.BoundType,
		Variant:         strPtr(in.TestPoint.Variant),
		Instance:        instance,
		MeasuredFrom:    sp.MeasuredFrom.UTC().Format(time.RFC3339),
		MeasuredTo:      sp.MeasuredTo.UTC().Format(time.RFC3339),
		Reproducibility: repro,
		Pricing:         pricingOut,
		Sweep:           sp.Points,
	}, nil
}

// buildInstanceInfo resolves vcpu/ram_gb (when a pricing snapshot is
// available to derive them from) and assembles the result's instance block.
func buildInstanceInfo(snapshot *pricing.Snapshot, pricingFetcherKey string, pi pricingInputs, cpuArch, engineVersion string) *instanceInfo {
	var vcpu, ramGB *float64
	if snapshot != nil && pi.instanceType != "" {
		if fn, ok := newVCPURAMFuncs()[pricingFetcherKey]; ok {
			if v, r, err := fn(snapshot.Items, pi.instanceType); err == nil {
				vcpu, ramGB = &v, &r
			}
		}
	}
	return &instanceInfo{
		InstanceType:   strPtr(pi.instanceType),
		VCPU:           vcpu,
		RAMGB:          ramGB,
		DiskGB:         pi.diskGB,
		IOPS:           pi.iops,
		ThroughputMbps: pi.throughputMbps,
		DiskType:       strPtr(pi.diskType),
		CPUArch:        strPtr(cpuArch),
		EngineVersion:  strPtr(engineVersion),
	}
}

// buildReproducibility builds reproducibility.command: the dbarenactl-level
// invocation that reproduces this exact test point, not a raw benchctl
// command. That sidesteps ever needing to redact a run-supplied value (e.g.
// supabase_org_id) from it, since no --set list is shown at all: every
// value that would have appeared there (warehouses, disk sizing, ...) is
// already reported in instance/sweep[].workload_parameters. The manifest
// path it names is enough to recover the scenario file too, so the result
// doesn't repeat it.
func buildReproducibility(tp *sweepstate.TestPoint, manifestPath, benchctlVersion, gotpcVersion string) reproducibility {
	absManifestPath, err := filepath.Abs(manifestPath)
	if err != nil {
		absManifestPath = manifestPath
	}
	testPointKey := manifest.TestPointDef{Tier: tp.Tier, BoundType: tp.BoundType, Variant: tp.Variant}.Key()
	command := fmt.Sprintf("dbarenactl run --candidate %s --test-point %s", repoRelativePath(absManifestPath), testPointKey)
	return reproducibility{
		BenchctlVersion:   strPtr(benchctlVersion),
		DbarenactlVersion: strPtr(version),
		LoadGenerator:     loadGeneratorInfo{Name: strPtr("https://github.com/supabase/go-tpc/"), Version: strPtr(gotpcVersion)},
		Command:           strPtr(command),
	}
}

// computePricing computes the result's pricing block and backfills
// tpm_per_dollar_month onto every sweep point in place. Returns (nil, nil)
// when there's no pricing snapshot -- pricing: null is expected, not an
// error. Returns an error only when a snapshot exists but no cost
// calculator is registered for pricingFetcherKey (a real configuration
// bug, and today the caller treats it as fatal for this test point); a
// per-run cost-computation failure is a warning (pricing: null), not an
// error.
func computePricing(snapshot *pricing.Snapshot, pricingFetcherKey string, pi pricingInputs, scenario string, points []sweepPointJSON) (*pricingInfo, error) {
	if snapshot == nil {
		return nil, nil
	}
	calc, ok := newCostCalculators()[pricingFetcherKey]
	if !ok {
		return nil, fmt.Errorf("%s: no cost calculator registered for %q", scenario, pricingFetcherKey)
	}
	costInput := pricing.CostInput{
		InstanceType: pi.instanceType, DiskType: pi.diskType,
		DiskBaselineIOPS: pi.diskBaselineIOPS, DiskBaselineThroughputMbps: pi.diskBaselineThroughput,
	}
	if pi.diskGB != nil {
		costInput.DiskGB = *pi.diskGB
	}
	if pi.iops != nil {
		costInput.IOPS = *pi.iops
	}
	if pi.throughputMbps != nil {
		costInput.ThroughputMbps = *pi.throughputMbps
	}
	if pi.dataCacheGB != nil {
		costInput.DataCacheGB = *pi.dataCacheGB
	}
	breakdown, err := calc.Cost(snapshot.Items, costInput)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s: could not compute pricing: %v -- writing pricing: null\n", scenario, err)
		return nil, nil
	}
	components := make([]costComponentJSON, len(breakdown.Components))
	for i, c := range breakdown.Components {
		components[i] = costComponentJSON{Name: c.Name, AmountUSD: c.AmountUSD, Detail: c.Detail}
	}
	pricingOut := &pricingInfo{
		MonthlyUSD: breakdown.TotalUSD, HoursPerMonth: pricing.HoursPerMonth,
		PricingModel: "on-demand-list-price",
		FetchedAt:    strPtr(snapshot.FetchedAt.UTC().Format(time.RFC3339)),
		Components:   components,
	}
	for i := range points {
		// tpm_per_dollar_month is a throughput/dollar ratio, not currency --
		// rounded to 2 decimal places (a separate policy from
		// pricing.MoneyDecimals) to drop float64 division noise without
		// implying more precision than the underlying measurement supports.
		v := pricing.RoundTo(points[i].Summary.Throughput.Value/pricingOut.MonthlyUSD, 2)
		points[i].Summary.TpmPerDollarMonth = &v
	}
	return pricingOut, nil
}

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dbarena/dbarenactl/internal/manifest"
	"github.com/dbarena/dbarenactl/internal/planner"
	"github.com/dbarena/dbarenactl/internal/scheduler"
	"github.com/dbarena/dbarenactl/internal/sweepid"
	"github.com/dbarena/dbarenactl/internal/sweepstate"
)

var (
	runCandidate           string
	runMaxConcurrency      int
	runIterations          int
	runMaxWorkloadFailures int
	runOnWorkloadFailure   string
	runDryRun              bool
	runBenchctlBin         string
	runSetParams           []string
)

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run all required benchmarks for a candidate manifest",
	Args:  cobra.NoArgs,
	RunE:  runRun,
}

func init() {
	runCmd.Flags().StringVar(&runCandidate, "candidate", "", "Path to a sweep manifest, e.g. candidates/aws-rds-tpcc.yaml (required)")
	runCmd.Flags().IntVar(&runMaxConcurrency, "max-concurrency", 1, "Maximum number of environments running at once")
	runCmd.Flags().IntVar(&runIterations, "iterations", 3, "Successful iterations required per test point; also the default failure budget (see --max-workload-failures)")
	runCmd.Flags().IntVar(&runMaxWorkloadFailures, "max-workload-failures", 0, "Override the failure budget independently of --iterations (0 = use --iterations)")
	runCmd.Flags().StringVar(&runOnWorkloadFailure, "on-workload-failure", "retry", "How to handle a failed (not just slow) workload: retry | fail-teardown")
	runCmd.Flags().BoolVar(&runDryRun, "dry-run", false, "Preview the benchctl invocations this sweep would make, without touching anything")
	runCmd.Flags().StringVar(&runBenchctlBin, "benchctl-bin", "benchctl", "Path to the benchctl binary")
	runCmd.Flags().StringArrayVar(&runSetParams, "set", nil, "Set a manifest parameter referenced as {{ params.NAME }} in the manifest (key=value, repeatable), e.g. --set supabase_org_id=abc1234")
	_ = runCmd.MarkFlagRequired("candidate")
}

// parseParams converts ["key=value", ...] from --set flags into a map,
// mirroring benchctl's own parseSetFlags (cmd/benchctl/run.go).
func parseParams(sets []string) (map[string]string, error) {
	params := make(map[string]string, len(sets))
	for _, s := range sets {
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("--set %q: expected key=value format", s)
		}
		params[k] = v
	}
	return params, nil
}

func runRun(cmd *cobra.Command, _ []string) error {
	failureBudget := runIterations
	switch runOnWorkloadFailure {
	case "retry":
		// failureBudget already defaults to --iterations.
	case "fail-teardown":
		// Stop the whole sweep on the very first workload failure: reuses
		// the same budget-exhaustion mechanism as --iterations, just with a
		// budget of 1 instead of --iterations.
		failureBudget = 1
	case "fail-keep":
		return fmt.Errorf("--on-workload-failure=fail-keep is not implemented yet")
	default:
		return fmt.Errorf("--on-workload-failure: unknown value %q (want retry or fail-teardown)", runOnWorkloadFailure)
	}
	if runMaxWorkloadFailures > 0 {
		failureBudget = runMaxWorkloadFailures
	}

	manifestContent, err := os.ReadFile(runCandidate)
	if err != nil {
		return fmt.Errorf("load candidate manifest %q: %w", runCandidate, err)
	}
	m, err := manifest.Load(runCandidate)
	if err != nil {
		return fmt.Errorf("load candidate manifest %q: %w", runCandidate, err)
	}

	manifestParams, err := parseParams(runSetParams)
	if err != nil {
		return err
	}
	if err := m.ResolveParams(manifestParams); err != nil {
		return err
	}

	sweepID := sweepid.Compute(sweepid.Params{
		Provider:            m.Provider,
		Workload:            m.Workload,
		ManifestContent:     manifestContent,
		MaxConcurrency:      runMaxConcurrency,
		Iterations:          runIterations,
		OnWorkloadFailure:   runOnWorkloadFailure,
		MaxWorkloadFailures: runMaxWorkloadFailures,
		ManifestParams:      manifestParams,
	})

	if runDryRun {
		return printDryRun(sweepID, m)
	}

	dbFile, err := dbPath()
	if err != nil {
		return err
	}
	store, err := sweepstate.Open(dbFile)
	if err != nil {
		return err
	}
	defer store.Close() //nolint:errcheck

	existing, err := store.GetSweep(sweepID)
	switch {
	case err == nil && existing.Status == sweepstate.SweepStoppedError && existing.ErrorAction == scheduler.ActionBudgetExhausted:
		// Deliberately not auto-continued: a budget-exhausted sweep needs an
		// interactive choice (continue vs. start fresh) that only `resume`
		// offers. Blindly calling executeSweep here would just immediately
		// re-hit the same dead-end error with no useful prompt.
		return fmt.Errorf(
			"sweep %s exhausted its failure budget on test point %s -- run `dbarenactl resume %s` to decide how to proceed",
			sweepID, existing.ErrorTarget, sweepID)
	case err == nil && existing.Status == sweepstate.SweepStoppedError:
		// Nothing is currently managing this sweep -- reaching stopped_error
		// only happens after the process that recorded it has returned (and
		// released the sweep's lock) or died outright, in which case the
		// lock is stale and reclaimable. Safe to just continue it, exactly
		// as `dbarenactl resume` would.
		printResumeNotice(existing)
		return executeSweep(cmd.Context(), store, sweepID, runBenchctlBin)
	case err == nil && existing.Status == sweepstate.SweepCompleted:
		// Identical provider/workload/manifest/flags to a sweep that already
		// finished -- e.g. re-running against a newly deployed version.
		// Nothing is running (a completed status is only ever reached after
		// the setting process released the sweep's lock), so it's safe to
		// discard the old test points/runs and start over under the same id.
		printFreshRestartNotice(sweepID)
		if err := store.ResetSweep(sweepID); err != nil {
			return err
		}
	case err == nil:
		return fmt.Errorf(
			"an incomplete sweep %s already exists for this provider and parameter set. Options:\n"+
				"  - `dbarenactl resume %s` to continue it\n"+
				"  - `dbarenactl delete %s` to delete it and start over\n"+
				"  - change a flag or the manifest to start a genuinely different sweep",
			sweepID, sweepID, sweepID)
	case err != nil && !errors.Is(err, sweepstate.ErrNotFound):
		return err
	}

	params := sweepParams{
		Provider: m.Provider, Workload: m.Workload, ManifestPath: runCandidate, MaxConcurrency: runMaxConcurrency,
		Iterations: runIterations, OnWorkloadFailure: runOnWorkloadFailure, MaxWorkloadFailures: runMaxWorkloadFailures,
	}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return err
	}
	sweep := &sweepstate.Sweep{ID: sweepID, Provider: m.Provider, Workload: m.Workload, ParamsJSON: string(paramsJSON), CreatedAt: time.Now().UTC()}
	if err := store.CreateSweep(sweep); err != nil {
		return err
	}
	if err := store.CreateTestPoints(planner.BuildTestPoints(sweepID, m, runIterations, failureBudget)); err != nil {
		return err
	}

	return executeSweep(cmd.Context(), store, sweepID, runBenchctlBin)
}

func printDryRun(sweepID string, m *manifest.Manifest) error {
	fmt.Printf("Sweep %s (dry run -- nothing will be launched or persisted)\n\n", sweepID)
	for _, c := range planner.PreviewFirstAttempts(sweepID, m) {
		fmt.Printf("[%s]\n  %s\n\n", c.TestPointKey, c.String())
	}
	fmt.Println("Each test point retries under a new run id (with the run's exact attempt")
	fmt.Println("count depending on runtime success/failure) until it reaches --iterations")
	fmt.Println("successes or exhausts its failure budget.")
	return nil
}

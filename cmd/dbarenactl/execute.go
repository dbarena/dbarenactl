package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dbarena/dbarenactl/internal/bench"
	"github.com/dbarena/dbarenactl/internal/lock"
	"github.com/dbarena/dbarenactl/internal/scheduler"
	"github.com/dbarena/dbarenactl/internal/sweepstate"
)

// defaultFetchRetryLimit and defaultPollInterval are not exposed as flags --
// they're implementation details of how the scheduler polls/retries, not
// part of a sweep's identity or something the methodology cares about.
const (
	defaultFetchRetryLimit = 3
	defaultPollInterval    = 30 * time.Second
)

// pollInterval returns defaultPollInterval, unless DBARENACTL_POLL_INTERVAL
// is set to a valid duration -- an undocumented override for driving the
// scheduler's poll loop faster in tests, never surfaced as a real flag.
func pollInterval() time.Duration {
	if v := os.Getenv("DBARENACTL_POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return defaultPollInterval
}

// sweepParams is everything about a sweep's invocation that `dbarenactl
// resume` needs to reconstruct later, since resume takes no flags of its
// own -- a sweep's parameters are fixed at creation time (see
// internal/sweepid: they're part of the sweep's identity).
type sweepParams struct {
	Provider            string `json:"provider"`
	Product             string `json:"product"`
	Plan                string `json:"plan"`
	Workload            string `json:"workload"`
	ManifestPath        string `json:"manifest_path"`
	MaxConcurrency      int    `json:"max_concurrency"`
	Iterations          int    `json:"iterations"`
	OnWorkloadFailure   string `json:"on_workload_failure"`
	MaxWorkloadFailures int    `json:"max_workload_failures"`
}

// printResumeNotice reports why a sweep previously stopped, before
// continuing it. Shared by `run` (when it finds an existing sweep stopped on
// an error and continues it instead of erroring) and `resume`.
func printResumeNotice(sweep *sweepstate.Sweep) {
	fmt.Fprintf(os.Stderr, "Resuming from a %s failure on %s:\n  %s\n\n", sweep.ErrorAction, sweep.ErrorTarget, sweep.ErrorDetail)
}

// printFreshRestartNotice reports that `run` found this exact sweep already
// completed and is starting a new attempt under the same id, discarding the
// previous attempt's test points and runs (see sweepstate.ResetSweep). The
// previous attempt's fetched result files aren't touched by that reset, so
// this also points at where they still are on disk.
func printFreshRestartNotice(sweepID string) {
	msg := fmt.Sprintf("Sweep %s already completed -- starting a fresh attempt (previous results are no longer tracked, but their files remain on disk", sweepID)
	if dir, err := artifactBaseDir(sweepID); err == nil {
		msg += " under " + dir
	}
	fmt.Fprintln(os.Stderr, msg+").")
}

// printBudgetExhaustedDiagnostic reports which test point stopped the sweep
// and where its failed runs' logs live, so the user can actually root-cause
// it instead of being told to "accept the shortfall" with no next step.
func printBudgetExhaustedDiagnostic(w io.Writer, store *sweepstate.Store, sweepID string, tp *sweepstate.TestPoint) error {
	fmt.Fprintf(w, "Sweep %s stopped as the test point %s exhausted its budget of %d failures.\n\n", sweepID, tp.ID, tp.FailureBudget)

	runs, err := store.ListRunsForTestPoint(tp.ID)
	if err != nil {
		return err
	}
	logDir, err := logsBaseDir(sweepID)
	if err != nil {
		return err
	}

	fmt.Fprintln(w, "Next steps:")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "1. Inspect the log files:")
	fmt.Fprintln(w)
	for _, r := range runs {
		if r.Outcome == "failure" {
			fmt.Fprintln(w, "   "+filepath.Join(logDir, r.RunID+".log"))
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "   to identify the root cause.")
	fmt.Fprintln(w)
	return nil
}

// executeSweep acquires the sweep's lock, builds the scheduler from the
// sweep's own recorded parameters, and runs it to completion or until it
// stops (ctx cancellation, or a stop-the-world condition). Shared by `run`
// (after creating a brand-new sweep, or continuing one it found already
// stopped on an error) and `resume` (against an existing one).
//
// concurrencyOverride, if > 0, replaces the sweep's stored MaxConcurrency for
// this and every future invocation (it's persisted back to params_json) --
// unlike the rest of a sweep's parameters, concurrency is a runtime knob, not
// part of its identity (see internal/sweepid). `run`'s call sites always
// pass 0 (no override): changing an existing sweep's concurrency is
// `resume`'s job.
func executeSweep(ctx context.Context, store *sweepstate.Store, sweepID, benchctlBin string, concurrencyOverride int) error {
	sweep, err := store.GetSweep(sweepID)
	if err != nil {
		return err
	}
	var params sweepParams
	if err := json.Unmarshal([]byte(sweep.ParamsJSON), &params); err != nil {
		return fmt.Errorf("sweep %s: corrupt params: %w", sweepID, err)
	}
	if concurrencyOverride > 0 && concurrencyOverride != params.MaxConcurrency {
		params.MaxConcurrency = concurrencyOverride
		paramsJSON, err := json.Marshal(params)
		if err != nil {
			return err
		}
		if err := store.UpdateParamsJSON(sweepID, string(paramsJSON)); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Overriding max-concurrency to %d for sweep %s (this sticks for future resumes too).\n", concurrencyOverride, sweepID)
	}

	lockFilePath, err := lockPath(sweepID)
	if err != nil {
		return err
	}
	l, err := lock.Acquire(lockFilePath)
	if err != nil {
		if errors.Is(err, lock.ErrHeld) {
			return fmt.Errorf("sweep %s is already being managed by another dbarenactl process: %w", sweepID, err)
		}
		return err
	}
	defer l.Release() //nolint:errcheck

	artifactDir, err := artifactBaseDir(sweepID)
	if err != nil {
		return err
	}
	logDir, err := logsBaseDir(sweepID)
	if err != nil {
		return err
	}

	benchClient := bench.New(benchctlBin)
	benchClient.LogDir = logDir
	sched := &scheduler.Scheduler{Store: store, Bench: benchClient, Out: os.Stderr}
	opts := scheduler.Options{
		MaxConcurrency:  params.MaxConcurrency,
		FetchRetryLimit: defaultFetchRetryLimit,
		ArtifactBaseDir: artifactDir,
		PollInterval:    pollInterval(),
	}

	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fmt.Fprintf(os.Stderr, "==> Sweep %s: running (provider=%s, product=%s, plan=%s, workload=%s, max-concurrency=%d)\n",
		sweepID, params.Provider, params.Product, params.Plan, params.Workload, params.MaxConcurrency)
	runErr := sched.RunSweep(ctx, sweepID, opts)
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) {
			fmt.Fprintf(os.Stderr, "\nInterrupted -- progress is saved. Run `dbarenactl resume %s` to continue.\n", sweepID)
			return nil
		}
		fmt.Fprintln(os.Stderr)
		if stopped, serr := store.GetSweep(sweepID); serr == nil && stopped.ErrorAction == scheduler.ActionBudgetExhausted {
			if tp, terr := store.GetTestPoint(stopped.ErrorTarget); terr == nil {
				if err := printBudgetExhaustedDiagnostic(os.Stderr, store, sweepID, tp); err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "Once resolved, run `dbarenactl resume %s` to continue from this point.\n", sweepID)
				return runErr
			}
		}
		fmt.Fprintf(os.Stderr, "Sweep %s stopped: %v\n", sweepID, runErr)
		fmt.Fprintf(os.Stderr, "Once resolved, run `dbarenactl resume %s` to continue from exactly this point.\n", sweepID)
		return runErr
	}
	fmt.Fprintf(os.Stderr, "==> Sweep %s complete.\n", sweepID)
	return nil
}

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

	"github.com/spf13/cobra"

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
func printResumeNotice(store *sweepstate.Store, sweep *sweepstate.Sweep) {
	fmt.Fprintf(os.Stderr, "Resuming from a %s failure on %s:\n  %s\n\n",
		sweep.ErrorAction, errorTargetLabel(store, sweep.ErrorTarget), sweep.ErrorDetail)
}

// errorTargetLabel renders a sweep's ErrorTarget -- a test point id for a
// launch failure, a run id for a fetch/teardown one (see
// sweepstate.Sweep) -- as the short name progress output uses. A run keeps
// its id alongside the label, since that's what gets handed to `benchctl
// status|connect|teardown`. Falls back to the raw target, which is always
// meaningful even if it is long.
func errorTargetLabel(store *sweepstate.Store, target string) string {
	if tp, err := store.GetTestPoint(target); err == nil {
		return tp.Label()
	}
	run, err := store.GetRun(target)
	if err != nil {
		return target
	}
	tp, err := store.GetTestPoint(run.TestPointID)
	if err != nil {
		return target
	}
	return fmt.Sprintf("%s (run %s)", tp.RunLabel(run.IterationAttempt), target)
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

// printSweepStoppedNotice reports why RunSweep stopped and what to do next.
// When runErr traces back to a *bench.RunError (a launch/fetch/teardown
// failure), it prints the action, run id, and log path as short, labeled
// fields instead of re-serializing the error's full "bench: <action> <runID>:
// ...(see ... for full output)" text, which the caller (executeSweep) or the
// run's own progress line has already printed once already. Anything else
// (e.g. a local store error) falls back to printing runErr as-is, since
// there's no structured detail to pull out.
func printSweepStoppedNotice(w io.Writer, sweepID string, runErr error) {
	var re *bench.RunError
	if errors.As(runErr, &re) {
		fmt.Fprintf(w, "Sweep %s stopped: %s failed (%v)\n", sweepID, re.Action, re.Err)
		fmt.Fprintf(w, "  Run:    %s\n", re.RunID)
		if re.LogPath != "" {
			fmt.Fprintf(w, "  Log:    %s\n", re.LogPath)
		}
		fmt.Fprintf(w, "  Resume: dbarenactl resume %s\n", sweepID)
		return
	}
	fmt.Fprintf(w, "Sweep %s stopped: %v\n", sweepID, runErr)
	fmt.Fprintf(w, "Once resolved, run `dbarenactl resume %s` to continue from exactly this point.\n", sweepID)
}

// printBudgetExhaustedDiagnostic reports which test point stopped the sweep
// and where its failed runs' logs live, so the user can actually root-cause
// it instead of being told to "accept the shortfall" with no next step.
func printBudgetExhaustedDiagnostic(w io.Writer, store *sweepstate.Store, sweepID string, tp *sweepstate.TestPoint) error {
	fmt.Fprintf(w, "Sweep %s stopped as the test point %s exhausted its budget of %d failures.\n\n", sweepID, tp.Label(), tp.FailureBudget)

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
func executeSweep(cmd *cobra.Command, store *sweepstate.Store, sweepID, benchctlBin string, concurrencyOverride int) error {
	sweep, err := store.GetSweep(sweepID)
	if err != nil {
		return err
	}
	if err := store.TouchLastStarted(sweepID, time.Now().UTC()); err != nil {
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

	ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fmt.Fprintf(os.Stderr, "==> Sweep %s: running (provider=%s, product=%s, plan=%s, workload=%s, max-concurrency=%d)\n",
		sweepID, params.Provider, params.Product, params.Plan, params.Workload, params.MaxConcurrency)
	// Progress lines below name test points as tier/bound-type[/variant]
	// #attempt rather than by run id, so point at where the run ids a
	// benchctl command needs still live.
	fmt.Fprintf(os.Stderr, "    Run ids for benchctl: dbarenactl status %s\n", sweepID)
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
				cmd.SilenceErrors = true
				return runErr
			}
		}
		printSweepStoppedNotice(os.Stderr, sweepID, runErr)
		cmd.SilenceErrors = true
		return runErr
	}
	fmt.Fprintf(os.Stderr, "==> Sweep %s complete.\n", sweepID)
	return nil
}

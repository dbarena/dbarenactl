package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
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

// executeSweep acquires the sweep's lock, builds the scheduler from the
// sweep's own recorded parameters, and runs it to completion or until it
// stops (ctx cancellation, or a stop-the-world condition). Shared by `run`
// (after creating a brand-new sweep, or continuing one it found already
// stopped on an error) and `resume` (against an existing one).
func executeSweep(ctx context.Context, store *sweepstate.Store, sweepID, benchctlBin string) error {
	sweep, err := store.GetSweep(sweepID)
	if err != nil {
		return err
	}
	var params sweepParams
	if err := json.Unmarshal([]byte(sweep.ParamsJSON), &params); err != nil {
		return fmt.Errorf("sweep %s: corrupt params: %w", sweepID, err)
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

	fmt.Fprintf(os.Stderr, "==> Sweep %s: running (provider=%s, workload=%s, max-concurrency=%d)\n", sweepID, params.Provider, params.Workload, params.MaxConcurrency)
	runErr := sched.RunSweep(ctx, sweepID, opts)
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) {
			fmt.Fprintf(os.Stderr, "\nInterrupted -- progress is saved. Run `dbarenactl resume %s` to continue.\n", sweepID)
			return nil
		}
		fmt.Fprintf(os.Stderr, "\nSweep %s stopped: %v\n", sweepID, runErr)
		fmt.Fprintf(os.Stderr, "Once resolved, run `dbarenactl resume %s` to continue from exactly this point.\n", sweepID)
		return runErr
	}
	fmt.Fprintf(os.Stderr, "==> Sweep %s complete.\n", sweepID)
	return nil
}

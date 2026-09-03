// Package scheduler is dbarenactl's run/resume control loop: it decides what
// to launch, reconciles in-flight runs against benchctl's own state, and
// implements the resilience policy the design settled on:
//
//   - any launch/teardown failure, or a results-pull that exhausts its
//     bounded local retries, stops the whole sweep immediately (no
//     provider-specific error classification) and records exactly what to
//     retry first on resume;
//   - a workload that ran but failed is expected and auto-retried, budgeted
//     independently of the successes a test point still needs;
//   - a run found "launching" at the start of a resume is orphan-detected
//     from benchctl's own state, not from a guess at timing;
//   - a stale heartbeat during workload.execute is only ever a warning,
//     never an automatic teardown.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/dbarena/dbarenactl/internal/bench"
	"github.com/dbarena/dbarenactl/internal/planner"
	"github.com/dbarena/dbarenactl/internal/sweepstate"
	"github.com/dbarena/dbarenactl/internal/ui"
)

// Sweep-wide error actions recorded in Sweep.ErrorAction. Distinguishes what
// kind of stop this was, purely for resume-time messaging -- the actual
// retry mechanics are the same normal reconciliation pass either way.
const (
	ActionLaunch          = "launch"
	ActionStatus          = "status"
	ActionTeardown        = "teardown"
	ActionFetch           = "fetch"
	ActionBudgetExhausted = "budget_exhausted"
)

// Options configures one sweep's execution.
type Options struct {
	MaxConcurrency  int
	SuccessesNeeded int
	FailureBudget   int
	// FetchRetryLimit bounds local retries of a failed results-pull before
	// escalating to the sweep-wide stop rule.
	FetchRetryLimit int
	// ArtifactBaseDir is where per-run artifacts are fetched to, one
	// subdirectory per run id.
	ArtifactBaseDir string
	// PollInterval is how long RunSweep waits between Step calls when a
	// pass makes no progress (e.g. every run is still legitimately
	// executing remotely).
	PollInterval time.Duration
}

// StepResult reports the outcome of one scheduling pass.
type StepResult struct {
	// Done is true once every test point in the sweep is satisfied.
	Done bool
	// Progressed is true if anything changed this pass (a run advanced
	// phase, a new one launched, etc). RunSweep uses this to decide whether
	// to poll again immediately or wait out PollInterval.
	Progressed bool
}

// Scheduler drives a single sweep's test points to completion.
type Scheduler struct {
	Store *sweepstate.Store
	Bench bench.Runner
	// Out receives human-readable progress/warning messages. Defaults to
	// io.Discard if nil.
	Out io.Writer
	// Now returns the current time; overridable in tests. Defaults to
	// time.Now.
	Now func() time.Time
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scheduler) logf(format string, args ...any) {
	if s.Out != nil {
		fmt.Fprintf(s.Out, "[%s] "+format+"\n", append([]any{s.timestamp()}, args...)...)
	}
}

// timestamp renders s.now() as an RFC3339 UTC timestamp, for stamping when a
// line first appears (logf's own lines, and a spinner's start/finish lines
// via stamp).
func (s *Scheduler) timestamp() string {
	return s.now().UTC().Format(time.RFC3339)
}

// stamp prefixes message with the current timestamp, for one-off spinner
// messages (Start/Succeed/Fail) that logf's own formatting doesn't cover.
func (s *Scheduler) stamp(message string) string {
	return fmt.Sprintf("[%s] %s", s.timestamp(), message)
}

// markf writes one stamped line prefixed with a "✓"/"✗" mark, for terminal
// events discovered by polling rather than by dbarenactl itself blocking on
// a call (e.g. workload completion) -- there's no local operation to spin
// through, but the outcome still deserves the same success/failure marker
// as the spinner-backed lines, for visual consistency.
func (s *Scheduler) markf(mark, format string, args ...any) {
	if s.Out == nil {
		return
	}
	fmt.Fprintln(s.Out, mark+" "+s.stamp(fmt.Sprintf(format, args...)))
}

// spinner returns a Spinner for one launch/teardown operation, writing
// through the same Out as logf (so production gets a real animated spinner
// on a terminal, and tests get the same plain start/end lines logf itself
// would produce).
func (s *Scheduler) spinner(message string) *ui.Spinner {
	out := s.Out
	if out == nil {
		out = io.Discard
	}
	return ui.NewWithWriter(out, s.stamp(message))
}

// RunSweep drives sweepID to completion, polling at opts.PollInterval when a
// pass makes no progress, until it's done, ctx is cancelled, or a stop-the-
// world condition is hit (returned as an error; sweepstate already records
// what to retry -- the caller should tell the user to run `dbarenactl resume`).
func (s *Scheduler) RunSweep(ctx context.Context, sweepID string, opts Options) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		result, err := s.Step(ctx, sweepID, opts)
		if err != nil {
			return err
		}
		if result.Done {
			return nil
		}
		if !result.Progressed {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(opts.PollInterval):
			}
		}
	}
}

// Step performs one scheduling pass: reconcile every non-terminal run
// against benchctl, then launch new runs for any test point with spare
// capacity. It is the unit of work RunSweep repeats, and the primary
// surface this package's tests exercise -- no sleeping, no looping, fully
// deterministic given a fake bench.Runner.
func (s *Scheduler) Step(ctx context.Context, sweepID string, opts Options) (StepResult, error) {
	sweep, err := s.Store.GetSweep(sweepID)
	if err != nil {
		return StepResult{}, err
	}
	if sweep.ErrorAction == ActionBudgetExhausted {
		return StepResult{}, budgetExhaustedError(sweep)
	}
	hadError := sweep.HasError()

	progressed := false

	runs, err := s.Store.ListNonTerminalRuns(sweepID)
	if err != nil {
		return StepResult{}, err
	}
	for _, run := range runs {
		p, err := s.reconcileRun(ctx, sweepID, run, opts)
		if err != nil {
			return StepResult{}, err
		}
		progressed = progressed || p
	}

	testPoints, err := s.Store.ListTestPoints(sweepID)
	if err != nil {
		return StepResult{}, err
	}
	// Re-list post-reconciliation: FinalizeRun above may have moved runs to
	// terminal, freeing capacity/test points that a stale in-memory list
	// would still show as busy.
	activeRuns, err := s.Store.ListNonTerminalRuns(sweepID)
	if err != nil {
		return StepResult{}, err
	}
	inFlight := len(activeRuns)
	pendingByTestPoint := make(map[string]int, len(activeRuns))
	for _, r := range activeRuns {
		pendingByTestPoint[r.TestPointID]++
	}

	allSatisfied := true
	var exhausted *sweepstate.TestPoint
	launchable := make([]*sweepstate.TestPoint, 0, len(testPoints))
	for _, tp := range testPoints {
		if tp.Satisfied() {
			continue
		}
		allSatisfied = false
		if tp.BudgetExhausted() {
			exhausted = tp
			continue
		}
		launchable = append(launchable, tp)
	}

	// Fill capacity in rounds, one attempt per still-eligible test point per
	// round, so spare capacity spreads breadth-first across test points
	// first and only stacks several concurrent attempts on the same test
	// point once every other one already has all the attempts it could use.
	for madeProgress := true; madeProgress && inFlight < opts.MaxConcurrency; {
		madeProgress = false
		for _, tp := range launchable {
			if inFlight >= opts.MaxConcurrency {
				break
			}
			// A test point can run several attempts concurrently -- there's
			// nothing shared between them, each provisions its own infra
			// under a distinct run id. Cap concurrent attempts at what
			// could still be needed (optimistically assuming in-flight ones
			// succeed), so a pass never launches more than the sweep will
			// end up wanting.
			remaining := tp.SuccessesNeeded - tp.SuccessesCount - pendingByTestPoint[tp.ID]
			if remaining <= 0 {
				continue
			}
			attempt := tp.SuccessesCount + tp.FailuresCount + pendingByTestPoint[tp.ID] + 1
			if err := s.launch(ctx, sweepID, tp, attempt); err != nil {
				return StepResult{}, err
			}
			inFlight++
			pendingByTestPoint[tp.ID]++
			progressed = true
			madeProgress = true
		}
	}

	if exhausted != nil {
		detail := fmt.Sprintf("test point %s exhausted its failure budget (%d/%d failures) without reaching %d successes",
			exhausted.ID, exhausted.FailuresCount, exhausted.FailureBudget, exhausted.SuccessesNeeded)
		if err := s.Store.RecordError(sweepID, ActionBudgetExhausted, exhausted.ID, detail); err != nil {
			return StepResult{}, err
		}
		sweep.ErrorAction, sweep.ErrorTarget, sweep.ErrorDetail = ActionBudgetExhausted, exhausted.ID, detail
		return StepResult{}, budgetExhaustedError(sweep)
	}

	if hadError {
		if err := s.Store.ClearError(sweepID); err != nil {
			return StepResult{}, err
		}
	}

	if allSatisfied {
		if err := s.Store.MarkCompleted(sweepID); err != nil {
			return StepResult{}, err
		}
		return StepResult{Done: true, Progressed: progressed}, nil
	}

	return StepResult{Progressed: progressed}, nil
}

func budgetExhaustedError(sweep *sweepstate.Sweep) error {
	return fmt.Errorf("%s -- this sweep cannot complete automatically; inspect it and either accept the shortfall, raise the failure budget, or start a new sweep", sweep.ErrorDetail)
}

func (s *Scheduler) reconcileRun(ctx context.Context, sweepID string, run *sweepstate.Run, opts Options) (bool, error) {
	switch run.Status {
	case sweepstate.RunLaunching:
		return s.reconcileLaunching(ctx, sweepID, run)
	case sweepstate.RunWaitingRemote:
		return s.reconcileWaitingRemote(ctx, sweepID, run)
	case sweepstate.RunNeedsResultsPull:
		return s.reconcileNeedsResultsPull(ctx, sweepID, run, opts)
	case sweepstate.RunNeedsTeardown:
		return s.reconcileNeedsTeardown(ctx, sweepID, run)
	default:
		return false, fmt.Errorf("scheduler: run %s: unexpected non-terminal status %q", run.RunID, run.Status)
	}
}

// launchingVerdict is the outcome of classifying a RunLaunching row found at
// the start of a scheduling pass (always either mid-launch-within-this-
// process, which resolves synchronously and is never seen here, or leftover
// from a prior crashed process).
type launchingVerdict int

const (
	verdictHandedOff launchingVerdict = iota
	verdictOrphaned
	verdictAmbiguous
)

// classifyLaunching decides what a RunLaunching row's real state is, purely
// from benchctl's own reported state plus how long ago dbarenactl recorded
// intent to launch it. Split out as a pure function so the narrow
// provision-completed-but-not-yet-handed-off grace window (see
// bench.HandoffGracePeriod) is testable without real sleeps.
func classifyLaunching(rs *bench.RunState, createdAt, now time.Time) launchingVerdict {
	if rs.HandedOff() {
		return verdictHandedOff
	}
	if !rs.ProvisionCompleted() {
		return verdictOrphaned
	}
	if now.Sub(createdAt) < bench.HandoffGracePeriod {
		return verdictAmbiguous
	}
	return verdictOrphaned
}

func (s *Scheduler) reconcileLaunching(ctx context.Context, sweepID string, run *sweepstate.Run) (bool, error) {
	rs, err := s.Bench.Status(ctx, run.RunID)
	if errors.Is(err, bench.ErrRunNotFound) {
		// benchctl's store never got a record at all -- the previous
		// attempt died before Store.Create, so nothing was provisioned.
		// Safe to discard outright; it never counted as a real attempt.
		return true, s.Store.DeleteRun(run.RunID)
	}
	if errors.Is(err, bench.ErrBenchctlUnusable) {
		if rerr := s.Store.RecordError(sweepID, ActionStatus, run.RunID, err.Error()); rerr != nil {
			return false, rerr
		}
		return false, fmt.Errorf("check status of %s: %w", run.RunID, err)
	}
	if err != nil {
		// A transient status-check problem (network, store auth) is not
		// itself proof of anything about the run -- don't stop the world
		// over it, just try again next pass.
		s.logf("warning: checking status of %s: %v", run.RunID, err)
		return false, nil
	}

	switch classifyLaunching(rs, run.CreatedAt, s.now()) {
	case verdictHandedOff:
		// It actually made it to the remote driver despite the local
		// crash. Update our bookkeeping to match reality and move on.
		return true, s.Store.SetRunStatus(run.RunID, sweepstate.RunWaitingRemote)
	case verdictAmbiguous:
		return false, nil
	}

	// Orphaned: local provisioning either never finished or never got
	// handed off in time. Infra may exist -- tear it down before discarding
	// the row so a fresh attempt gets a clean slate.
	sp := s.spinner(fmt.Sprintf("%s: launch left an orphaned environment; tearing down before retrying", run.RunID))
	sp.Start()
	start := time.Now()
	defer func() { sp.Fail(s.stamp(fmt.Sprintf("%s: orphaned teardown did not finish", run.RunID))) }()
	if err := s.Bench.Teardown(ctx, run.RunID); err != nil {
		sp.Fail(s.stamp(fmt.Sprintf("%s: tear down orphaned environment failed: %v", run.RunID, err)))
		if rerr := s.Store.RecordError(sweepID, ActionTeardown, run.RunID, err.Error()); rerr != nil {
			return false, rerr
		}
		return false, fmt.Errorf("tear down orphaned run %s: %w", run.RunID, err)
	}
	sp.Succeed(s.stamp(fmt.Sprintf("%s: orphaned environment torn down in %s", run.RunID, time.Since(start).Round(time.Second))))
	return true, s.Store.DeleteRun(run.RunID)
}

func (s *Scheduler) reconcileWaitingRemote(ctx context.Context, sweepID string, run *sweepstate.Run) (bool, error) {
	rs, err := s.Bench.Status(ctx, run.RunID)
	if errors.Is(err, bench.ErrBenchctlUnusable) {
		if rerr := s.Store.RecordError(sweepID, ActionStatus, run.RunID, err.Error()); rerr != nil {
			return false, rerr
		}
		return false, fmt.Errorf("check status of %s: %w", run.RunID, err)
	}
	if err != nil {
		if errors.Is(err, bench.ErrRunNotFound) {
			s.logf("warning: run %s vanished from benchctl's state store", run.RunID)
		} else {
			s.logf("warning: checking status of %s: %v", run.RunID, err)
		}
		return false, nil
	}

	if rs.Stale(s.now()) {
		s.logf("warning: run %s has been stale for over %s -- may be dead, or alive but unable to report (see benchctl's state-store docs on driver-side token expiry). Not tearing down automatically; investigate with `benchctl connect %s driver`.",
			run.RunID, bench.StaleThreshold, run.RunID)
	}

	if rs.CompletedAt == nil {
		if rs.TerminatedWithoutCompleting() {
			s.markf("✗", "%s: environment terminated without the workload completing after %s -- finalizing as failed",
				run.RunID, s.now().Sub(run.CreatedAt).Round(time.Second))
			return true, s.Store.FinalizeRun(run.RunID, "failure")
		}
		return false, nil
	}

	outcome, mark := "success", "✓"
	if rs.Error != "" {
		outcome, mark = "failure", "✗"
	}
	if err := s.Store.SetRunOutcome(run.RunID, outcome); err != nil {
		return false, err
	}
	// run.CreatedAt is when dbarenactl first committed to launching this run
	// (see launch()) -- the easiest available proxy for "since the
	// environment was launched." Workloads run for hours to days, so
	// rounding to the second is plenty accurate.
	s.markf(mark, "%s: workload finished (%s) after %s", run.RunID, outcome, s.now().Sub(run.CreatedAt).Round(time.Second))
	return true, s.Store.SetRunStatus(run.RunID, sweepstate.RunNeedsResultsPull)
}

func (s *Scheduler) reconcileNeedsResultsPull(ctx context.Context, sweepID string, run *sweepstate.Run, opts Options) (bool, error) {
	rs, statusErr := s.Bench.Status(ctx, run.RunID)
	if errors.Is(statusErr, bench.ErrBenchctlUnusable) {
		if rerr := s.Store.RecordError(sweepID, ActionStatus, run.RunID, statusErr.Error()); rerr != nil {
			return false, rerr
		}
		return false, fmt.Errorf("check status of %s: %w", run.RunID, statusErr)
	}
	if statusErr == nil && rs.TerminatedAt != nil {
		// The environment came down (e.g. a manual `benchctl teardown` on a
		// run stuck for good) before results were ever pulled -- no retry
		// will succeed against infra that no longer exists.
		s.logf("%s: environment already torn down -- results can no longer be fetched, finalizing as failed", run.RunID)
		return true, s.Store.FinalizeRun(run.RunID, "failure")
	}

	dest := filepath.Join(opts.ArtifactBaseDir, run.RunID)
	sp := s.spinner(fmt.Sprintf("%s: fetching results", run.RunID))
	sp.Start()
	start := time.Now()
	defer func() { sp.Fail(s.stamp(fmt.Sprintf("%s: fetch did not finish", run.RunID))) }()
	fetchErr := s.Bench.Fetch(ctx, run.RunID, dest)
	if fetchErr == nil {
		sp.Succeed(s.stamp(fmt.Sprintf("%s: results fetched in %s", run.RunID, time.Since(start).Round(time.Second))))
		if err := s.Store.SetRunArtifactDir(run.RunID, dest); err != nil {
			return false, err
		}
		return true, s.Store.SetRunStatus(run.RunID, sweepstate.RunNeedsTeardown)
	}

	if errors.Is(fetchErr, bench.ErrBenchctlUnusable) {
		sp.Fail(s.stamp(fmt.Sprintf("%s: fetch failed: %v", run.RunID, fetchErr)))
		if rerr := s.Store.RecordError(sweepID, ActionFetch, run.RunID, fetchErr.Error()); rerr != nil {
			return false, rerr
		}
		return false, fmt.Errorf("fetch %s: %w", run.RunID, fetchErr)
	}

	n, err := s.Store.IncrementFetchAttempts(run.RunID)
	if err != nil {
		sp.Fail(s.stamp(fmt.Sprintf("%s: fetch failed: %v", run.RunID, fetchErr)))
		return false, err
	}
	if n < opts.FetchRetryLimit {
		sp.Fail(s.stamp(fmt.Sprintf("%s: fetch failed (attempt %d/%d), will retry: %v", run.RunID, n, opts.FetchRetryLimit, fetchErr)))
		return false, nil
	}
	sp.Fail(s.stamp(fmt.Sprintf("%s: fetch failed permanently after %d attempts: %v", run.RunID, n, fetchErr)))
	detail := fmt.Sprintf("results-pull failed %d times: %v", n, fetchErr)
	if rerr := s.Store.RecordError(sweepID, ActionFetch, run.RunID, detail); rerr != nil {
		return false, rerr
	}
	return false, errors.New(detail)
}

func (s *Scheduler) reconcileNeedsTeardown(ctx context.Context, sweepID string, run *sweepstate.Run) (bool, error) {
	sp := s.spinner(fmt.Sprintf("%s: tearing down", run.RunID))
	sp.Start()
	start := time.Now()
	defer func() { sp.Fail(s.stamp(fmt.Sprintf("%s: teardown did not finish", run.RunID))) }()
	if err := s.Bench.Teardown(ctx, run.RunID); err != nil {
		sp.Fail(s.stamp(fmt.Sprintf("%s: tear down failed: %v", run.RunID, err)))
		if rerr := s.Store.RecordError(sweepID, ActionTeardown, run.RunID, err.Error()); rerr != nil {
			return false, rerr
		}
		return false, fmt.Errorf("tear down %s: %w", run.RunID, err)
	}
	if err := s.Store.FinalizeRun(run.RunID, run.Outcome); err != nil {
		sp.Fail(s.stamp(fmt.Sprintf("%s: torn down, but finalizing failed: %v", run.RunID, err)))
		return false, err
	}
	elapsed := time.Since(start).Round(time.Second)
	msg := fmt.Sprintf("%s: torn down in %s", run.RunID, elapsed)
	if tp, err := s.Store.GetTestPoint(run.TestPointID); err == nil {
		msg = fmt.Sprintf("%s: torn down in %s -- %s now %d/%d successes, %d/%d failures",
			run.RunID, elapsed, run.TestPointID, tp.SuccessesCount, tp.SuccessesNeeded, tp.FailuresCount, tp.FailureBudget)
	}
	sp.Succeed(s.stamp(msg))
	return true, nil
}

func (s *Scheduler) launch(ctx context.Context, sweepID string, tp *sweepstate.TestPoint, attempt int) error {
	runID := planner.NewRunID(tp.ID, attempt)
	run := &sweepstate.Run{RunID: runID, TestPointID: tp.ID, IterationAttempt: attempt}
	// Durably committed *before* invoking benchctl at all -- this is what
	// lets a crash mid-provisioning be detected and cleaned up later,
	// instead of leaking cloud resources under a run id nobody local knows
	// about.
	if err := s.Store.CreateRun(run); err != nil {
		return err
	}
	sp := s.spinner(fmt.Sprintf("launching %s (run %s, attempt %d)", tp.ID, runID, attempt))
	sp.Start()
	start := time.Now()
	defer func() { sp.Fail(s.stamp(fmt.Sprintf("%s: launch did not finish", runID))) }()

	if err := s.Bench.LaunchAsync(ctx, runID, tp.Scenario, tp.Set); err != nil {
		sp.Fail(s.stamp(fmt.Sprintf("launch %s failed: %v", runID, err)))
		if rerr := s.Store.RecordError(sweepID, ActionLaunch, runID, err.Error()); rerr != nil {
			return rerr
		}
		return fmt.Errorf("launch %s: %w", runID, err)
	}
	sp.Succeed(s.stamp(fmt.Sprintf("%s: provisioned in %s, workload running remotely", runID, time.Since(start).Round(time.Second))))
	return s.Store.SetRunStatus(runID, sweepstate.RunWaitingRemote)
}

// RestartExhaustedSweep discards a budget-exhausted sweep's entire progress
// so it can start completely over. It tears down infra for *every*
// non-terminal run in the sweep first, not just the recorded exhausted test
// point -- a launch can race in on a different, still-healthy test point in
// the very same Step call that trips the budget (Step reconciles, then
// scans for exhaustion, then launches new work, all in one pass), leaving
// that other test point's run genuinely non-terminal at the moment the
// sweep stops. Deleting its row without tearing it down first would
// silently orphan whatever infra it represents.
func (s *Scheduler) RestartExhaustedSweep(ctx context.Context, sweepID string) error {
	sweep, err := s.Store.GetSweep(sweepID)
	if err != nil {
		return err
	}
	if sweep.ErrorAction != ActionBudgetExhausted {
		return fmt.Errorf("scheduler: restart sweep %s: not stopped on a budget-exhausted test point (action=%q)", sweepID, sweep.ErrorAction)
	}

	runs, err := s.Store.ListNonTerminalRuns(sweepID)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if err := s.Bench.Teardown(ctx, run.RunID); err != nil {
			if rerr := s.Store.RecordError(sweepID, ActionTeardown, run.RunID, err.Error()); rerr != nil {
				return rerr
			}
			return fmt.Errorf("tear down %s before restart: %w", run.RunID, err)
		}
	}

	return s.Store.RestartSweep(sweepID)
}

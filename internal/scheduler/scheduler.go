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
//   - a stale heartbeat during workload.execute fails the run and tears its
//     environment down. The signal cannot be disambiguated from here (see
//     failStaleRun) and either cause leaves benchctl's view of the run frozen,
//     so the run would otherwise hold its slot forever.
//
// Provisioning is the one operation that blocks for tens of minutes (project
// create, disk resize, tofu apply, ssh bootstrap), so it runs on a background
// goroutine -- one at a time, so local load and provider API pressure are
// unchanged -- while the control loop keeps reconciling. That buys the
// scheduler's central cost invariant: an environment whose workload has
// finished is fetched and torn down within one poll interval, and no new
// environment is ever launched while a finished one is still waiting to be
// drained. Getting this wrong is expensive -- a finished environment bills
// for a project and a load-driver instance while producing nothing.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
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

	// mu guards the launcher bookkeeping below.
	mu sync.Mutex
	// launching holds the run ids whose provisioning goroutine is still
	// running in *this* process. reconcileLaunching consults it so a live
	// provision is never mistaken for a crashed one (see classifyLaunching).
	launching map[string]struct{}
	// failure records the first launch failure a goroutine hit, for the next
	// Step to pick up and escalate. Goroutines deliberately never touch
	// sweep-level error state themselves -- keeping that single-writer is
	// what lets Step's hadError/ClearError logic stay race-free.
	failure *launchFailure
	// wg tracks in-flight launch goroutines so RunSweep can wait them out.
	wg sync.WaitGroup

	// outOnce/out lazily wrap Out so the control loop, a fetch/teardown
	// spinner and a background launch goroutine can all write to it without
	// interleaving halfway through a line.
	outOnce sync.Once
	out     io.Writer
}

// syncWriter serializes concurrent writes to one underlying writer. fmt's
// Fprint* family issues a single Write per call, so locking here makes every
// log line and every spinner frame atomic with respect to the others.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

// syncFile is a syncWriter over an *os.File that keeps Fd visible, so
// wrapping os.Stderr doesn't hide the terminal from ui.NewWithWriter's TTY
// detection and downgrade every spinner to plain lines.
type syncFile struct {
	*syncWriter
	f *os.File
}

func (w *syncFile) Fd() uintptr { return w.f.Fd() }

// writer returns Out wrapped for concurrent use, or io.Discard if Out is nil.
func (s *Scheduler) writer() io.Writer {
	s.outOnce.Do(func() {
		switch w := s.Out; v := w.(type) {
		case nil:
			s.out = io.Discard
		case *os.File:
			s.out = &syncFile{syncWriter: &syncWriter{w: v}, f: v}
		default:
			s.out = &syncWriter{w: w}
		}
	})
	return s.out
}

// launchFailure is a launch that failed on a background goroutine, waiting to
// be escalated to the sweep-wide stop rule by the next Step.
type launchFailure struct {
	runID string
	err   error
}

// isLaunching reports whether runID's provisioning goroutine is still running
// in this process.
func (s *Scheduler) isLaunching(runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.launching[runID]
	return ok
}

// launchBusy reports whether any provision is currently in flight. Launches
// are deliberately serialized: running several tofu applies and provider CLI
// calls at once multiplies local load and risks provider API rate limits, for
// no gain -- capacity fills at the same rate either way.
func (s *Scheduler) launchBusy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.launching) > 0
}

// takeLaunchFailure removes and returns the pending launch failure, if any.
func (s *Scheduler) takeLaunchFailure() *launchFailure {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.failure
	s.failure = nil
	return f
}

// WaitForLaunches blocks until every in-flight launch goroutine has returned.
// Callers that want them to stop promptly should cancel the context passed to
// Step first.
func (s *Scheduler) WaitForLaunches() { s.wg.Wait() }

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scheduler) logf(format string, args ...any) {
	if s.Out != nil {
		fmt.Fprintf(s.writer(), "[%s] "+format+"\n", append([]any{s.timestamp()}, args...)...)
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
	fmt.Fprintln(s.writer(), mark+" "+s.stamp(fmt.Sprintf(format, args...)))
}

// runLabel renders run as its test point's short name plus which attempt
// this is, e.g. "2xlarge/cache-fit/performance-optimized #2" -- what every
// progress line is keyed by, in place of the run id it used to print. Falls
// back to the run id if the test point can't be loaded: a line named a
// little less readably beats no line at all.
//
// One indexed read of a local SQLite file per message is cheap enough not to
// warrant caching, and the store is already used concurrently (see
// launchAsync's goroutine).
func (s *Scheduler) runLabel(run *sweepstate.Run) string {
	tp, err := s.Store.GetTestPoint(run.TestPointID)
	if err != nil {
		return run.RunID
	}
	return tp.RunLabel(run.IterationAttempt)
}

// testPointLabel renders a test point id as its short tier/bound-type
// [/variant] name, falling back to the id itself if it can't be loaded.
func (s *Scheduler) testPointLabel(testPointID string) string {
	tp, err := s.Store.GetTestPoint(testPointID)
	if err != nil {
		return testPointID
	}
	return tp.Label()
}

// spinner returns a Spinner for one fetch/teardown operation, writing
// through the same Out as logf (so production gets a real animated spinner
// on a terminal, and tests get the same plain start/end lines logf itself
// would produce). Launches don't use one: they run on a background
// goroutine, and see launchAsync for why that rules a spinner out.
func (s *Scheduler) spinner(message string) *ui.Spinner {
	return ui.NewWithWriter(s.writer(), s.stamp(message))
}

// RunSweep drives sweepID to completion, polling at opts.PollInterval when a
// pass makes no progress, until it's done, ctx is cancelled, or a stop-the-
// world condition is hit (returned as an error; sweepstate already records
// what to retry -- the caller should tell the user to run `dbarenactl resume`).
func (s *Scheduler) RunSweep(ctx context.Context, sweepID string, opts Options) error {
	// Launch goroutines get their own cancellable context so that returning
	// -- for any reason -- stops a provision in progress instead of waiting
	// out a tofu apply that can run for the better part of an hour after the
	// sweep has already given up. Cancelling kills the benchctl child, whose
	// own teardown defer unwinds what it created; anything it still leaves
	// behind is caught by reconcileLaunching's orphan path on the next
	// `dbarenactl resume`.
	ctx, cancelLaunches := context.WithCancel(ctx)
	defer func() {
		cancelLaunches()
		if s.launchBusy() {
			s.logf("stopping: cancelling the launch still in flight -- `dbarenactl resume %s` will tear down anything it left behind", sweepID)
		}
		s.WaitForLaunches()
	}()

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
	// Before anything else: a launch that failed on a background goroutine
	// is the same stop-the-world condition a synchronous one was, just
	// observed a pass later. Recording it here rather than in the goroutine
	// keeps sweep-level error state single-writer.
	if f := s.takeLaunchFailure(); f != nil {
		if rerr := s.Store.RecordError(sweepID, ActionLaunch, f.runID, failureDetail(f.err)); rerr != nil {
			return StepResult{}, rerr
		}
		return StepResult{}, f.err
	}

	sweep, err := s.Store.GetSweep(sweepID)
	if err != nil {
		return StepResult{}, err
	}
	if sweep.ErrorAction == ActionBudgetExhausted {
		return StepResult{}, budgetExhaustedError(sweep, s.testPointLabel(sweep.ErrorTarget))
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
	draining := false
	for _, r := range activeRuns {
		pendingByTestPoint[r.TestPointID]++
		if r.Status == sweepstate.RunNeedsResultsPull || r.Status == sweepstate.RunNeedsTeardown {
			draining = true
		}
	}

	allSatisfied := true
	var exhausted *sweepstate.TestPoint
	launchable := make([]*sweepstate.TestPoint, 0, len(testPoints))
	for _, tp := range testPoints {
		if tp.Satisfied() || tp.Skipped {
			continue
		}
		allSatisfied = false
		if tp.BudgetExhausted() {
			exhausted = tp
			continue
		}
		launchable = append(launchable, tp)
	}

	// At most one launch per pass, and only once nothing is left to drain.
	// Both conditions exist for the same reason: provisioning blocks for
	// tens of minutes, so anything queued behind it -- another launch, or a
	// finished environment's fetch and teardown -- would wait that long too.
	// Reconciliation above has already drained everything it could, so a
	// draining run here is one whose fetch hit a transient failure; it gets
	// retried next pass, bounded by opts.FetchRetryLimit.
	next := nextToLaunch(launchable, pendingByTestPoint)
	switch {
	case next == nil || s.launchBusy() || inFlight >= opts.MaxConcurrency:
		// Nothing wants a slot, a provision is already running, or the
		// sweep is at capacity.
	case draining:
		s.logf("holding off on launching %s until every finished environment is torn down", next.Label())
	default:
		attempt := next.SuccessesCount + next.FailuresCount + pendingByTestPoint[next.ID] + 1
		if err := s.launchAsync(ctx, next, attempt); err != nil {
			return StepResult{}, err
		}
		inFlight++
		progressed = true
	}

	if exhausted != nil {
		detail := fmt.Sprintf("test point %s exhausted its failure budget (%d/%d failures) without reaching %d successes",
			exhausted.Label(), exhausted.FailuresCount, exhausted.FailureBudget, exhausted.SuccessesNeeded)
		if err := s.Store.RecordError(sweepID, ActionBudgetExhausted, exhausted.ID, detail); err != nil {
			return StepResult{}, err
		}
		sweep.ErrorAction, sweep.ErrorTarget, sweep.ErrorDetail = ActionBudgetExhausted, exhausted.ID, detail
		return StepResult{}, budgetExhaustedError(sweep, exhausted.Label())
	}

	if hadError {
		if err := s.Store.ClearError(sweepID); err != nil {
			return StepResult{}, err
		}
	}

	// inFlight reflects any pre-existing non-terminal run, straggler or not:
	// when allSatisfied is true, launchable is necessarily empty (every test
	// point is either Satisfied or Skipped), so the launch loop above added
	// nothing to it. Gating completion on inFlight == 0 stops a sweep from
	// being marked done while a concurrent attempt for an already-satisfied
	// or now-skipped test point is still executing -- once a sweep is
	// SweepCompleted, nothing ever calls Step for it again, which would
	// otherwise orphan that straggler's environment for good.
	if allSatisfied && inFlight == 0 {
		if err := s.Store.MarkCompleted(sweepID); err != nil {
			return StepResult{}, err
		}
		return StepResult{Done: true, Progressed: progressed}, nil
	}

	return StepResult{Progressed: progressed}, nil
}

func budgetExhaustedError(sweep *sweepstate.Sweep, label string) error {
	return fmt.Errorf("test point %s exhausted its failure budget -- see above for next steps, or run `dbarenactl resume %s` to inspect and decide", label, sweep.ID)
}

func (s *Scheduler) reconcileRun(ctx context.Context, sweepID string, run *sweepstate.Run, opts Options) (bool, error) {
	switch run.Status {
	case sweepstate.RunLaunching:
		return s.reconcileLaunching(ctx, sweepID, run)
	case sweepstate.RunWaitingRemote:
		return s.reconcileWaitingRemote(ctx, sweepID, run, opts)
	case sweepstate.RunNeedsResultsPull:
		return s.reconcileNeedsResultsPull(ctx, sweepID, run, opts)
	case sweepstate.RunNeedsTeardown:
		return s.reconcileNeedsTeardown(ctx, sweepID, run)
	default:
		return false, fmt.Errorf("scheduler: run %s: unexpected non-terminal status %q", run.RunID, run.Status)
	}
}

// launchingVerdict is the outcome of classifying a RunLaunching row found at
// the start of a scheduling pass. Rows whose launch goroutine is still
// running in this process are filtered out by reconcileLaunching before they
// get here, so anything classified is leftover from a crashed or cancelled
// prior attempt.
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
	// Its provisioning goroutine is still running right here: benchctl has
	// only written "provision: running" so far, which classifyLaunching
	// would read as an orphan and tear down under a live tofu apply.
	if s.isLaunching(run.RunID) {
		return false, nil
	}

	rs, err := s.Bench.Status(ctx, run.RunID)
	if errors.Is(err, bench.ErrRunNotFound) {
		// benchctl's store never got a record at all -- the previous
		// attempt died before Store.Create, so nothing was provisioned.
		// Safe to discard outright; it never counted as a real attempt.
		return true, s.Store.DeleteRun(run.RunID)
	}
	label := s.runLabel(run)
	if errors.Is(err, bench.ErrBenchctlUnusable) {
		if rerr := s.Store.RecordError(sweepID, ActionStatus, run.RunID, failureDetail(err)); rerr != nil {
			return false, rerr
		}
		return false, fmt.Errorf("check status of %s (run %s): %w", label, run.RunID, err)
	}
	if err != nil {
		// A transient status-check problem (network, store auth) is not
		// itself proof of anything about the run -- don't stop the world
		// over it, just try again next pass.
		s.logf("warning: %s (run %s): checking status: %v", label, run.RunID, err)
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
	ref := fmt.Sprintf("%s (run %s)", label, run.RunID)
	sp := s.spinner(fmt.Sprintf("%s: launch left an orphaned environment; tearing down before retrying", ref))
	sp.Start()
	start := time.Now()
	defer func() { sp.Fail(s.stamp(fmt.Sprintf("%s: orphaned teardown did not finish", ref))) }()
	if err := s.Bench.Teardown(ctx, run.RunID); err != nil {
		sp.Fail(s.stamp(fmt.Sprintf("%s: tear down orphaned environment failed: %s", ref, failureDetail(err))))
		if rerr := s.Store.RecordError(sweepID, ActionTeardown, run.RunID, failureDetail(err)); rerr != nil {
			return false, rerr
		}
		return false, fmt.Errorf("tear down orphaned run %s: %w", ref, err)
	}
	sp.Succeed(s.stamp(fmt.Sprintf("%s: orphaned environment torn down in %s", ref, time.Since(start).Round(time.Second))))
	return true, s.Store.DeleteRun(run.RunID)
}

func (s *Scheduler) reconcileWaitingRemote(ctx context.Context, sweepID string, run *sweepstate.Run, opts Options) (bool, error) {
	rs, err := s.Bench.Status(ctx, run.RunID)
	label := s.runLabel(run)
	if errors.Is(err, bench.ErrBenchctlUnusable) {
		if rerr := s.Store.RecordError(sweepID, ActionStatus, run.RunID, failureDetail(err)); rerr != nil {
			return false, rerr
		}
		return false, fmt.Errorf("check status of %s (run %s): %w", label, run.RunID, err)
	}
	if err != nil {
		if errors.Is(err, bench.ErrRunNotFound) {
			s.logf("warning: %s (run %s) vanished from benchctl's state store", label, run.RunID)
		} else {
			s.logf("warning: %s (run %s): checking status: %v", label, run.RunID, err)
		}
		return false, nil
	}

	if rs.CompletedAt == nil {
		if rs.TerminatedWithoutCompleting() {
			s.markf("✗", "%s (run %s): environment terminated without the workload completing after %s -- finalizing as failed",
				label, run.RunID, s.now().Sub(run.CreatedAt).Round(time.Second))
			return true, s.Store.FinalizeRun(run.RunID, "failure")
		}
		// Checked after TerminatedWithoutCompleting: if the environment is
		// already gone, that branch finalizes without a teardown, and calling
		// Teardown on torn-down infrastructure would stop the whole sweep.
		if rs.Stale(s.now()) {
			return s.failStaleRun(ctx, run, label, opts)
		}
		return false, nil
	}

	// A failed workload is the one outcome the user may want to take to
	// benchctl, so it names the run id; a success needs no follow-up.
	outcome, mark, ref := "success", "✓", label
	if rs.Error != "" {
		outcome, mark, ref = "failure", "✗", fmt.Sprintf("%s (run %s)", label, run.RunID)
	}
	if err := s.Store.SetRunOutcome(run.RunID, outcome); err != nil {
		return false, err
	}
	// run.CreatedAt is when dbarenactl first committed to launching this run
	// (see launch()) -- the easiest available proxy for "since the
	// environment was launched." Workloads run for hours to days, so
	// rounding to the second is plenty accurate.
	s.markf(mark, "%s: workload finished (%s) after %s", ref, outcome, s.now().Sub(run.CreatedAt).Round(time.Second))
	return true, s.Store.SetRunStatus(run.RunID, sweepstate.RunNeedsResultsPull)
}

// failStaleRun finalizes a run that stopped reporting heartbeats. The signal is
// ambiguous: the driver may be dead, or alive but unable to report after its
// store token expired and dbarenactl cannot tell the two apart.
//
// Either way benchctl's view of the run is frozen, so it can never reach a
// terminal state on its own. Therefore the run is terminated.
func (s *Scheduler) failStaleRun(ctx context.Context, run *sweepstate.Run, label string, opts Options) (bool, error) {
	s.markf("✗", "%s (run %s): no heartbeat for over %s -- finalizing as failed and tearing down",
		label, run.RunID, bench.StaleThreshold)

	// Best effort: the driver's resume.log and any partial results are the only
	// evidence of why it stopped reporting, and they die with the instance. A
	// fetch failure is the expected case when the driver is gone, so unlike
	// reconcileNeedsResultsPull's fetch it must never stop the sweep.
	dest := filepath.Join(opts.ArtifactBaseDir, run.RunID)
	if err := s.Bench.Fetch(ctx, run.RunID, dest); err != nil {
		s.logf("%s (run %s): could not fetch diagnostics from the stale run: %s", label, run.RunID, failureDetail(err))
	} else if err := s.Store.SetRunArtifactDir(run.RunID, dest); err != nil {
		return false, err
	}

	// Recording the outcome up front leaves the teardown itself to
	// reconcileNeedsTeardown, which finalizes with run.Outcome and so charges
	// the test point's failure budget. "failure" also keeps the partial
	// artifacts out of the results (see cmd/dbarenactl/results.go), while
	// leaving them on disk to diagnose.
	if err := s.Store.SetRunOutcome(run.RunID, "failure"); err != nil {
		return false, err
	}
	return true, s.Store.SetRunStatus(run.RunID, sweepstate.RunNeedsTeardown)
}

func (s *Scheduler) reconcileNeedsResultsPull(ctx context.Context, sweepID string, run *sweepstate.Run, opts Options) (bool, error) {
	rs, statusErr := s.Bench.Status(ctx, run.RunID)
	label := s.runLabel(run)
	ref := fmt.Sprintf("%s (run %s)", label, run.RunID)
	if errors.Is(statusErr, bench.ErrBenchctlUnusable) {
		if rerr := s.Store.RecordError(sweepID, ActionStatus, run.RunID, failureDetail(statusErr)); rerr != nil {
			return false, rerr
		}
		return false, fmt.Errorf("check status of %s: %w", ref, statusErr)
	}
	if statusErr == nil && rs.TerminatedAt != nil {
		// The environment came down (e.g. a manual `benchctl teardown` on a
		// run stuck for good) before results were ever pulled -- no retry
		// will succeed against infra that no longer exists.
		s.logf("%s: environment already torn down -- results can no longer be fetched, finalizing as failed", ref)
		return true, s.Store.FinalizeRun(run.RunID, "failure")
	}

	dest := filepath.Join(opts.ArtifactBaseDir, run.RunID)
	sp := s.spinner(fmt.Sprintf("%s: fetching results", label))
	sp.Start()
	start := time.Now()
	defer func() { sp.Fail(s.stamp(fmt.Sprintf("%s: fetch did not finish", ref))) }()
	fetchErr := s.Bench.Fetch(ctx, run.RunID, dest)
	if fetchErr == nil {
		sp.Succeed(s.stamp(fmt.Sprintf("%s: results fetched in %s", label, time.Since(start).Round(time.Second))))
		if err := s.Store.SetRunArtifactDir(run.RunID, dest); err != nil {
			return false, err
		}
		return true, s.Store.SetRunStatus(run.RunID, sweepstate.RunNeedsTeardown)
	}

	if errors.Is(fetchErr, bench.ErrBenchctlUnusable) {
		sp.Fail(s.stamp(fmt.Sprintf("%s: fetch failed: %s", ref, failureDetail(fetchErr))))
		if rerr := s.Store.RecordError(sweepID, ActionFetch, run.RunID, failureDetail(fetchErr)); rerr != nil {
			return false, rerr
		}
		return false, fmt.Errorf("fetch %s: %w", ref, fetchErr)
	}

	n, err := s.Store.IncrementFetchAttempts(run.RunID)
	if err != nil {
		sp.Fail(s.stamp(fmt.Sprintf("%s: fetch failed: %s", ref, failureDetail(fetchErr))))
		return false, err
	}
	if n < opts.FetchRetryLimit {
		sp.Fail(s.stamp(fmt.Sprintf("%s: fetch failed (attempt %d/%d), will retry: %s", ref, n, opts.FetchRetryLimit, failureDetail(fetchErr))))
		return false, nil
	}
	sp.Fail(s.stamp(fmt.Sprintf("%s: fetch failed permanently after %d attempts: %s", ref, n, failureDetail(fetchErr))))
	detail := fmt.Sprintf("results-pull failed %d times: %s", n, failureDetail(fetchErr))
	if rerr := s.Store.RecordError(sweepID, ActionFetch, run.RunID, detail); rerr != nil {
		return false, rerr
	}
	return false, errors.New(detail)
}

func (s *Scheduler) reconcileNeedsTeardown(ctx context.Context, sweepID string, run *sweepstate.Run) (bool, error) {
	// Check first whether the environment is already gone -- e.g. someone
	// tore it down by hand after a previous teardown attempt failed, per
	// docs/troubleshooting.md's own advice. Skipping straight to Teardown in
	// that case would just fail again (there's nothing left to destroy) and
	// stop the sweep forever, since this run's status never changes on its
	// own. Mirrors reconcileNeedsResultsPull's identical guard.
	rs, statusErr := s.Bench.Status(ctx, run.RunID)
	label := s.runLabel(run)
	ref := fmt.Sprintf("%s (run %s)", label, run.RunID)
	if errors.Is(statusErr, bench.ErrBenchctlUnusable) {
		if rerr := s.Store.RecordError(sweepID, ActionStatus, run.RunID, failureDetail(statusErr)); rerr != nil {
			return false, rerr
		}
		return false, fmt.Errorf("check status of %s: %w", ref, statusErr)
	}
	alreadyGone := errors.Is(statusErr, bench.ErrRunNotFound) || (statusErr == nil && rs.TerminatedAt != nil)
	if statusErr != nil && !alreadyGone {
		s.logf("warning: %s: checking status before teardown: %v", ref, statusErr)
	}

	sp := s.spinner(fmt.Sprintf("%s: tearing down", label))
	sp.Start()
	start := time.Now()
	defer func() { sp.Fail(s.stamp(fmt.Sprintf("%s: teardown did not finish", ref))) }()
	if !alreadyGone {
		if err := s.Bench.Teardown(ctx, run.RunID); err != nil {
			sp.Fail(s.stamp(fmt.Sprintf("%s: tear down failed: %s", ref, failureDetail(err))))
			if rerr := s.Store.RecordError(sweepID, ActionTeardown, run.RunID, failureDetail(err)); rerr != nil {
				return false, rerr
			}
			return false, fmt.Errorf("tear down %s: %w", ref, err)
		}
	}
	if err := s.Store.FinalizeRun(run.RunID, run.Outcome); err != nil {
		sp.Fail(s.stamp(fmt.Sprintf("%s: torn down, but finalizing failed: %v", ref, err)))
		return false, err
	}
	elapsed := time.Since(start).Round(time.Second)
	msg := fmt.Sprintf("%s: torn down in %s", label, elapsed)
	if alreadyGone {
		msg = fmt.Sprintf("%s: environment already gone -- nothing to tear down", label)
	}
	// The label already names the test point, so the tally just says "now".
	if tp, err := s.Store.GetTestPoint(run.TestPointID); err == nil {
		msg = fmt.Sprintf("%s -- now %d/%d successes, %d/%d failures",
			msg, tp.SuccessesCount, tp.SuccessesNeeded, tp.FailuresCount, tp.FailureBudget)
	}
	sp.Succeed(s.stamp(msg))
	return true, nil
}

// nextToLaunch picks the single test point to start an attempt for, or nil
// if none can use one. Choosing the eligible test point with the fewest
// attempts started so far -- completed (success or failure) or still in
// flight -- spreads attempts breadth-first across every test point in the
// sweep, not just within one pass's concurrency budget: every test point
// gets its first attempt before any gets a second, its second before any
// gets a third, and so on. This holds even at max-concurrency 1, where at
// most one attempt is ever in flight, because completed attempts count too.
// Breadth-first matters here because the sweep's ETA (see cmd/dbarenactl
// status.go) can't be computed until every remaining test point has at
// least one completed run to estimate from -- finishing one test point
// entirely before starting its neighbors would delay that indefinitely.
func nextToLaunch(launchable []*sweepstate.TestPoint, pendingByTestPoint map[string]int) *sweepstate.TestPoint {
	var best *sweepstate.TestPoint
	var bestAttempts int
	for _, tp := range launchable {
		// A test point can run several attempts concurrently -- there's
		// nothing shared between them, each provisions its own infra under a
		// distinct run id. Cap concurrent attempts at what could still be
		// needed (optimistically assuming in-flight ones succeed), so the
		// sweep never launches more than it will end up wanting.
		if tp.SuccessesNeeded-tp.SuccessesCount-pendingByTestPoint[tp.ID] <= 0 {
			continue
		}
		attempts := tp.SuccessesCount + tp.FailuresCount + pendingByTestPoint[tp.ID]
		if best == nil || attempts < bestAttempts {
			best, bestAttempts = tp, attempts
		}
	}
	return best
}

// launchAsync starts provisioning one attempt for tp on a background
// goroutine and returns immediately, so the control loop keeps reconciling --
// and so finished environments keep getting torn down -- while a provision
// that can run for tens of minutes is in flight. A failure is parked in
// s.failure for the next Step to escalate rather than returned here.
//
// Progress is reported with plain log lines rather than a ui.Spinner: a
// spinner animating from a background goroutine would interleave its
// carriage-return frames with everything else the loop writes to the same
// stderr.
func (s *Scheduler) launchAsync(ctx context.Context, tp *sweepstate.TestPoint, attempt int) error {
	runID := planner.NewRunID(tp.ID, attempt)
	run := &sweepstate.Run{RunID: runID, TestPointID: tp.ID, IterationAttempt: attempt}
	// Durably committed *before* invoking benchctl at all -- this is what
	// lets a crash mid-provisioning be detected and cleaned up later,
	// instead of leaking cloud resources under a run id nobody local knows
	// about.
	if err := s.Store.CreateRun(run); err != nil {
		return err
	}

	s.mu.Lock()
	if s.launching == nil {
		s.launching = make(map[string]struct{})
	}
	s.launching[runID] = struct{}{}
	s.mu.Unlock()

	label := tp.RunLabel(attempt)
	s.logf("launching %s", label)
	start := s.now()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		err := s.Bench.LaunchAsync(ctx, runID, tp.Scenario, tp.Set)
		if err == nil {
			err = s.Store.SetRunStatus(runID, sweepstate.RunWaitingRemote)
		}

		s.mu.Lock()
		delete(s.launching, runID)
		// First failure wins: the sweep stops on it either way, and the
		// first one is the one that explains the rest.
		if err != nil && s.failure == nil {
			s.failure = &launchFailure{runID: runID, err: err}
		}
		s.mu.Unlock()

		// The run row stays at RunLaunching on failure, which is exactly
		// what reconcileLaunching's orphan detection expects to find.
		if err != nil {
			s.markf("✗", "%s (run %s): launch failed after %s: %s",
				label, runID, s.now().Sub(start).Round(time.Second), failureDetail(err))
			return
		}
		s.markf("✓", "%s: provisioned in %s, workload running remotely",
			label, s.now().Sub(start).Round(time.Second))
	}()
	return nil
}

// failureDetail renders a benchctl error's cause and (if known) log path
// without re-serializing its full "bench: <action> <runID>: ..." prefix --
// every line that prints it already names the action and the run id, so
// repeating them here would be the same information twice in one sentence.
// Falls back to err.Error() for anything that isn't a *bench.RunError (e.g.
// a fake Runner in tests, or ctx cancellation).
func failureDetail(err error) string {
	var re *bench.RunError
	if errors.As(err, &re) {
		if re.LogPath != "" {
			return fmt.Sprintf("%v (see %s for full output)", re.Err, re.LogPath)
		}
		return re.Err.Error()
	}
	return err.Error()
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
			if rerr := s.Store.RecordError(sweepID, ActionTeardown, run.RunID, failureDetail(err)); rerr != nil {
				return rerr
			}
			return fmt.Errorf("tear down %s before restart: %w", run.RunID, err)
		}
	}

	return s.Store.RestartSweep(sweepID)
}

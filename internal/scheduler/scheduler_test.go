package scheduler

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbarena/dbarenactl/internal/bench"
	"github.com/dbarena/dbarenactl/internal/sweepstate"
)

func newTestScheduler(t *testing.T) (*Scheduler, *fakeBench, *sweepstate.Store) {
	t.Helper()
	st, err := sweepstate.Open(filepath.Join(t.TempDir(), "dbarenactl.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	fb := newFakeBench()
	var out strings.Builder
	s := &Scheduler{Store: st, Bench: fb, Out: &out}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("scheduler output:\n%s", out.String())
		}
	})
	return s, fb, st
}

func seedSweepWithOneTestPoint(t *testing.T, st *sweepstate.Store, successesNeeded, failureBudget int) (*sweepstate.Sweep, *sweepstate.TestPoint) {
	t.Helper()
	sw := &sweepstate.Sweep{ID: "sweep-1", Provider: "AWS", ParamsJSON: "{}", CreatedAt: time.Now().UTC()}
	if err := st.CreateSweep(sw); err != nil {
		t.Fatal(err)
	}
	tp := &sweepstate.TestPoint{
		ID: "sweep-1-small-io", SweepID: sw.ID, Tier: "small", Workload: "tpcc", Scenario: "x.yaml",
		BoundType: "io", Set: map[string]string{"project_size": "small"},
		SuccessesNeeded: successesNeeded, FailureBudget: failureBudget,
	}
	if err := st.CreateTestPoints([]*sweepstate.TestPoint{tp}); err != nil {
		t.Fatal(err)
	}
	return sw, tp
}

// step runs one scheduling pass and then waits out any launch it started, so
// a test can assert on a launch's outcome in the same place production
// observes it one pass later. Tests that exercise the asynchrony itself call
// s.Step directly.
func step(s *Scheduler, ctx context.Context, sweepID string, opts Options) (StepResult, error) {
	res, err := s.Step(ctx, sweepID, opts)
	s.WaitForLaunches()
	return res, err
}

// fill drives scheduling passes until no further launch starts, returning
// the first pass's result. Production only ever provisions one environment
// at a time, so a test that cares about the steady-state fleet rather than
// the number of passes it took to get there uses this.
func fill(t *testing.T, s *Scheduler, fb *fakeBench, ctx context.Context, sweepID string, opts Options) (StepResult, error) {
	t.Helper()
	var first StepResult
	for i := 0; i <= opts.MaxConcurrency; i++ {
		before := len(fb.launchCalls)
		res, err := step(s, ctx, sweepID, opts)
		if i == 0 {
			first = res
		}
		if err != nil || len(fb.launchCalls) == before {
			return first, err
		}
	}
	t.Fatalf("fill: still launching after %d passes", opts.MaxConcurrency+1)
	return first, nil
}

func defaultOpts() Options {
	return Options{
		MaxConcurrency:  6,
		FetchRetryLimit: 3,
		ArtifactBaseDir: "/tmp/dbarenactl-test-artifacts",
		PollInterval:    time.Millisecond,
	}
}

// completeRun advances a fakeRun all the way to a finished (successful or
// failed) workload, as if benchctl had run it to completion. Callers reach
// it through fakeBench.complete, which holds the fake's lock.
func completeRun(r *fakeRun, success bool) {
	r.state.Phases = map[string]string{
		bench.PhaseProvision:       "completed",
		bench.PhaseDriverSetup:     "completed",
		bench.PhaseWorkloadPrepare: "completed",
		bench.PhaseWorkloadExecute: "completed",
		bench.PhaseDriverCollect:   "completed",
	}
	now := time.Now().UTC()
	r.state.CompletedAt = &now
	if !success {
		r.state.Error = "workload exited with status 1"
	}
}

func TestStep_LaunchesNewRunForFreshTestPoint(t *testing.T) {
	s, fb, _ := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, s.Store, 1, 1)

	res, err := step(s, context.Background(), "sweep-1", defaultOpts())
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if res.Done || !res.Progressed {
		t.Errorf("res = %+v", res)
	}
	if len(fb.launchCalls) != 1 {
		t.Fatalf("launchCalls = %v", fb.launchCalls)
	}
	run, err := s.Store.GetRun(fb.launchCalls[0])
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != sweepstate.RunWaitingRemote {
		t.Errorf("run.Status = %q, want waiting_remote", run.Status)
	}
}

func TestStep_FullHappyPathToCompletion(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 1, 1)
	opts := defaultOpts()
	ctx := context.Background()

	// Pass 1: launch.
	if _, err := step(s, ctx, "sweep-1", opts); err != nil {
		t.Fatalf("launch step: %v", err)
	}
	runID := fb.launchCalls[0]

	// Pass 2: still executing -- no progress expected.
	res, err := step(s, ctx, "sweep-1", opts)
	if err != nil {
		t.Fatalf("waiting step: %v", err)
	}
	if res.Progressed || res.Done {
		t.Errorf("res while still executing = %+v", res)
	}

	// Workload finishes successfully.
	fb.complete(runID, true)
	res, err = step(s, ctx, "sweep-1", opts)
	if err != nil {
		t.Fatalf("completion step: %v", err)
	}
	if !res.Progressed || res.Done {
		t.Errorf("res after completion = %+v", res)
	}
	run, _ := s.Store.GetRun(runID)
	if run.Status != sweepstate.RunNeedsResultsPull {
		t.Errorf("run.Status = %q", run.Status)
	}

	// Fetch step.
	res, err = step(s, ctx, "sweep-1", opts)
	if err != nil {
		t.Fatalf("fetch step: %v", err)
	}
	if len(fb.fetchCalls) != 1 || fb.fetchCalls[0] != runID {
		t.Errorf("fetchCalls = %v", fb.fetchCalls)
	}
	run, _ = s.Store.GetRun(runID)
	if run.Status != sweepstate.RunNeedsTeardown {
		t.Errorf("run.Status = %q", run.Status)
	}

	// Teardown step -- should finalize and complete the sweep.
	res, err = step(s, ctx, "sweep-1", opts)
	if err != nil {
		t.Fatalf("teardown step: %v", err)
	}
	if len(fb.teardownCalls) != 1 {
		t.Errorf("teardownCalls = %v", fb.teardownCalls)
	}
	if !res.Done {
		t.Errorf("res after teardown = %+v, want Done", res)
	}

	sw, _ := st.GetSweep("sweep-1")
	if sw.Status != sweepstate.SweepCompleted {
		t.Errorf("sweep status = %q", sw.Status)
	}
	tp, _ := st.GetTestPoint("sweep-1-small-io")
	if tp.SuccessesCount != 1 {
		t.Errorf("SuccessesCount = %d", tp.SuccessesCount)
	}
}

func TestStep_LogsLaunchFinishAndTeardownProgress(t *testing.T) {
	st, err := sweepstate.Open(filepath.Join(t.TempDir(), "dbarenactl.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() }) //nolint:errcheck
	fb := newFakeBench()
	var out strings.Builder
	s := &Scheduler{Store: st, Bench: fb, Out: &out}

	seedSweepWithOneTestPoint(t, st, 1, 1)
	opts := defaultOpts()
	ctx := context.Background()

	if _, err := step(s, ctx, "sweep-1", opts); err != nil {
		t.Fatalf("launch step: %v", err)
	}
	runID := fb.launchCalls[0]
	if !strings.Contains(out.String(), "launching small/io (run "+runID) {
		t.Errorf("output missing launch line: %q", out.String())
	}
	if !strings.Contains(out.String(), "(run "+runID) || !strings.Contains(out.String(), "): provisioned") {
		t.Errorf("output missing provisioned line: %q", out.String())
	}

	fb.complete(runID, true)
	if _, err := step(s, ctx, "sweep-1", opts); err != nil {
		t.Fatalf("completion step: %v", err)
	}
	if !strings.Contains(out.String(), runID+": workload finished (success)") {
		t.Errorf("output missing finished line: %q", out.String())
	}

	if _, err := step(s, ctx, "sweep-1", opts); err != nil { // fetch
		t.Fatalf("fetch step: %v", err)
	}
	if !strings.Contains(out.String(), runID+": fetching results") {
		t.Errorf("output missing fetching line: %q", out.String())
	}
	if _, err := step(s, ctx, "sweep-1", opts); err != nil { // teardown + finalize
		t.Fatalf("teardown step: %v", err)
	}
	tearingDownIdx := strings.Index(out.String(), runID+": tearing down")
	tornDownIdx := strings.Index(out.String(), runID+": torn down in ")
	if tearingDownIdx == -1 {
		t.Errorf("output missing tearing-down line: %q", out.String())
	}
	if tornDownIdx == -1 {
		t.Errorf("output missing teardown line: %q", out.String())
	}
	if !strings.Contains(out.String(), "-- sweep-1-small-io now 1/1 successes, 0/1 failures") {
		t.Errorf("output missing test point tally: %q", out.String())
	}
	if tearingDownIdx != -1 && tornDownIdx != -1 && tearingDownIdx > tornDownIdx {
		t.Errorf("expected \"tearing down\" to precede \"torn down\": %q", out.String())
	}
}

func TestStep_LaunchFailureStopsTheWorld(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 1, 1)

	fb.onLaunch = func(runID string) { fb.run(runID).launchErr = errors.New("AWS credentials expired") }

	// The launch runs on a background goroutine, so the pass that starts it
	// returns cleanly; the failure it parked is escalated by the next one.
	if _, err := step(s, context.Background(), "sweep-1", defaultOpts()); err != nil {
		t.Fatalf("launching pass: %v", err)
	}
	_, err := step(s, context.Background(), "sweep-1", defaultOpts())
	if err == nil || !strings.Contains(err.Error(), "AWS credentials expired") {
		t.Fatalf("err = %v", err)
	}

	sw, _ := st.GetSweep("sweep-1")
	if sw.ErrorAction != ActionLaunch || !sw.HasError() {
		t.Errorf("sweep = %+v", sw)
	}

	run, err := st.GetRun(fb.launchCalls[0])
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != sweepstate.RunLaunching {
		t.Errorf("run.Status = %q, want still launching", run.Status)
	}
}

func TestStep_LaunchingStatusCheckUnusableBinary_StopsTheWorld(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 1, 1)

	orphanID := "sweep-1-small-io-1-deadbeef"
	if err := st.CreateRun(&sweepstate.Run{RunID: orphanID, TestPointID: "sweep-1-small-io", IterationAttempt: 1}); err != nil {
		t.Fatal(err)
	}
	fb.run(orphanID).statusErr = fmt.Errorf("%w: exec: \"benchctl\": executable file not found in $PATH", bench.ErrBenchctlUnusable)

	_, err := step(s, context.Background(), "sweep-1", defaultOpts())
	if err == nil || !errors.Is(err, bench.ErrBenchctlUnusable) {
		t.Fatalf("err = %v, want wrapping bench.ErrBenchctlUnusable", err)
	}

	sw, _ := st.GetSweep("sweep-1")
	if sw.ErrorAction != ActionStatus || !sw.HasError() {
		t.Errorf("sweep = %+v", sw)
	}
}

func TestStep_WaitingRemoteStatusCheckUnusableBinary_StopsTheWorld(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 1, 1)

	runID := "sweep-1-small-io-1-deadbeef"
	if err := st.CreateRun(&sweepstate.Run{RunID: runID, TestPointID: "sweep-1-small-io", IterationAttempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunStatus(runID, sweepstate.RunWaitingRemote); err != nil {
		t.Fatal(err)
	}
	fb.run(runID).statusErr = fmt.Errorf("%w: fork/exec /bad/path/benchctl: permission denied", bench.ErrBenchctlUnusable)

	_, err := step(s, context.Background(), "sweep-1", defaultOpts())
	if err == nil || !errors.Is(err, bench.ErrBenchctlUnusable) {
		t.Fatalf("err = %v, want wrapping bench.ErrBenchctlUnusable", err)
	}

	sw, _ := st.GetSweep("sweep-1")
	if sw.ErrorAction != ActionStatus || !sw.HasError() {
		t.Errorf("sweep = %+v", sw)
	}
}

func TestStep_FetchUnusableBinary_StopsTheWorldWithoutRetrying(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 1, 1)

	runID := "sweep-1-small-io-1-deadbeef"
	if err := st.CreateRun(&sweepstate.Run{RunID: runID, TestPointID: "sweep-1-small-io", IterationAttempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunStatus(runID, sweepstate.RunNeedsResultsPull); err != nil {
		t.Fatal(err)
	}
	fb.run(runID).fetchErr = fmt.Errorf("%w: exec: \"benchctl\": executable file not found in $PATH", bench.ErrBenchctlUnusable)

	_, err := step(s, context.Background(), "sweep-1", defaultOpts())
	if err == nil || !errors.Is(err, bench.ErrBenchctlUnusable) {
		t.Fatalf("err = %v, want wrapping bench.ErrBenchctlUnusable", err)
	}
	if len(fb.fetchCalls) != 1 {
		t.Errorf("expected exactly one fetch attempt (no pointless retries), got %v", fb.fetchCalls)
	}

	sw, _ := st.GetSweep("sweep-1")
	if sw.ErrorAction != ActionFetch || !sw.HasError() {
		t.Errorf("sweep = %+v", sw)
	}
}

func TestStep_ResumeAfterLaunchFailure_NeverProvisioned_DiscardsAndRetries(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 1, 1)

	// Simulate a crash: a run row stuck in "launching" whose subprocess
	// never got far enough to create a benchctl record at all.
	orphanID := "sweep-1-small-io-1-deadbeef"
	if err := st.CreateRun(&sweepstate.Run{RunID: orphanID, TestPointID: "sweep-1-small-io", IterationAttempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordError("sweep-1", ActionLaunch, orphanID, "process killed"); err != nil {
		t.Fatal(err)
	}
	fb.run(orphanID).notFound = true

	res, err := step(s, context.Background(), "sweep-1", defaultOpts())
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if !res.Progressed {
		t.Error("expected progress (discard + fresh launch)")
	}

	if _, err := st.GetRun(orphanID); !errors.Is(err, sweepstate.ErrNotFound) {
		t.Errorf("orphan run should have been deleted, got err=%v", err)
	}
	if len(fb.teardownCalls) != 0 {
		t.Errorf("no teardown expected for a never-provisioned orphan, got %v", fb.teardownCalls)
	}
	// A fresh launch should happen in the same pass, since the test point
	// now has spare capacity again.
	if len(fb.launchCalls) != 1 || fb.launchCalls[0] == orphanID {
		t.Errorf("expected exactly one fresh launch, got %v", fb.launchCalls)
	}

	sw, _ := st.GetSweep("sweep-1")
	if sw.HasError() {
		t.Errorf("sweep error should be cleared after successful resume pass: %+v", sw)
	}
}

func TestStep_ResumeAfterCrash_ProvisionNotCompleted_TearsDownAndRetries(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 1, 1)

	orphanID := "sweep-1-small-io-1-deadbeef"
	if err := st.CreateRun(&sweepstate.Run{RunID: orphanID, TestPointID: "sweep-1-small-io", IterationAttempt: 1}); err != nil {
		t.Fatal(err)
	}
	fb.run(orphanID).state.Phases[bench.PhaseProvision] = "running" // still provisioning when the crash hit

	res, err := step(s, context.Background(), "sweep-1", defaultOpts())
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if !res.Progressed {
		t.Error("expected progress")
	}
	if len(fb.teardownCalls) != 1 || fb.teardownCalls[0] != orphanID {
		t.Errorf("expected teardown of the orphan, got %v", fb.teardownCalls)
	}
	if _, err := st.GetRun(orphanID); !errors.Is(err, sweepstate.ErrNotFound) {
		t.Error("orphan run should have been deleted after teardown")
	}
	if len(fb.launchCalls) != 1 {
		t.Errorf("expected a fresh launch, got %v", fb.launchCalls)
	}

	out := s.Out.(*strings.Builder).String()
	if !strings.Contains(out, orphanID+": launch left an orphaned environment; tearing down before retrying") {
		t.Errorf("output missing orphan-teardown-starting line: %q", out)
	}
	if !strings.Contains(out, orphanID+": orphaned environment torn down") {
		t.Errorf("output missing orphan-teardown-finished line: %q", out)
	}
}

func TestStep_ResumeAfterCrash_ActuallyHandedOff_NotTreatedAsOrphan(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 1, 1)

	survivorID := "sweep-1-small-io-1-deadbeef"
	if err := st.CreateRun(&sweepstate.Run{RunID: survivorID, TestPointID: "sweep-1-small-io", IterationAttempt: 1}); err != nil {
		t.Fatal(err)
	}
	fb.run(survivorID).state.Phases = map[string]string{
		bench.PhaseProvision:   "completed",
		bench.PhaseDriverSetup: "running",
	}

	res, err := step(s, context.Background(), "sweep-1", defaultOpts())
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if !res.Progressed {
		t.Error("expected progress (status transition)")
	}
	if len(fb.teardownCalls) != 0 {
		t.Errorf("should not tear down a run that actually handed off, got %v", fb.teardownCalls)
	}
	if len(fb.launchCalls) != 0 {
		t.Errorf("should not launch a replacement for a run that's still legitimately active, got %v", fb.launchCalls)
	}
	run, err := st.GetRun(survivorID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != sweepstate.RunWaitingRemote {
		t.Errorf("run.Status = %q, want waiting_remote", run.Status)
	}
}

func TestStep_ResumeAfterCrash_AmbiguousWindow_WaitsWithoutActing(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 1, 1)

	ambiguousID := "sweep-1-small-io-1-deadbeef"
	if err := st.CreateRun(&sweepstate.Run{RunID: ambiguousID, TestPointID: "sweep-1-small-io", IterationAttempt: 1}); err != nil {
		t.Fatal(err)
	}
	// provision completed, nothing later reported yet, and CreatedAt is
	// "now" -- squarely inside the grace period.
	fb.run(ambiguousID).state.Phases[bench.PhaseProvision] = "completed"

	res, err := step(s, context.Background(), "sweep-1", defaultOpts())
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if res.Progressed {
		t.Error("an ambiguous run should not produce progress either way")
	}
	if len(fb.teardownCalls) != 0 || len(fb.launchCalls) != 0 {
		t.Errorf("no teardown/launch expected while ambiguous: teardown=%v launch=%v", fb.teardownCalls, fb.launchCalls)
	}
	run, err := st.GetRun(ambiguousID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != sweepstate.RunLaunching {
		t.Errorf("run.Status = %q, want unchanged (still launching)", run.Status)
	}
}

func TestStep_WorkloadFailure_AutoRetriesWithoutStoppingSweep(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	// Needs 2 successes, tolerates up to 2 failures -- room for one failed
	// attempt plus two successful ones.
	seedSweepWithOneTestPoint(t, st, 2, 2)
	opts := defaultOpts()
	ctx := context.Background()

	// With spare concurrency and 2 successes needed, both attempts stack
	// concurrently on the same test point.
	if _, err := fill(t, s, fb, ctx, "sweep-1", opts); err != nil {
		t.Fatal(err)
	}
	if len(fb.launchCalls) != 2 {
		t.Fatalf("launchCalls once filled = %v, want 2 (both needed successes stacked concurrently)", fb.launchCalls)
	}
	firstRunID := fb.launchCalls[0]
	fb.complete(firstRunID, false) // first attempt fails; second still in flight

	if _, err := step(s, ctx, "sweep-1", opts); err != nil { // -> needs_results_pull
		t.Fatal(err)
	}
	if _, err := step(s, ctx, "sweep-1", opts); err != nil { // -> needs_teardown (fetch)
		t.Fatal(err)
	}
	res, err := step(s, ctx, "sweep-1", opts) // teardown + finalize as failure
	if err != nil {
		t.Fatalf("teardown step: %v", err)
	}
	if res.Done {
		t.Fatal("sweep should not be done after a single failed attempt with successes still needed")
	}

	tp, err := st.GetTestPoint("sweep-1-small-io")
	if err != nil {
		t.Fatal(err)
	}
	if tp.FailuresCount != 1 || tp.SuccessesCount != 0 {
		t.Errorf("tp = %+v", tp)
	}

	// A fresh attempt should have been queued automatically in the same pass
	// that finalized the failure (capacity freed up, test point not
	// satisfied, budget not exhausted) -- alongside the still-running second
	// attempt from the first pass.
	if len(fb.launchCalls) != 3 {
		t.Fatalf("launchCalls = %v, want 3 (two original + retry)", fb.launchCalls)
	}
	sw, _ := st.GetSweep("sweep-1")
	if sw.HasError() {
		t.Errorf("a normal workload failure must not stop the sweep: %+v", sw)
	}
}

func TestStep_FailureBudgetExhausted_StopsTheWorldAndStaysExhausted(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 5, 1) // only 1 failure tolerated
	opts := defaultOpts()
	ctx := context.Background()

	if _, err := step(s, ctx, "sweep-1", opts); err != nil {
		t.Fatal(err)
	}
	runID := fb.launchCalls[0]
	fb.complete(runID, false)
	if _, err := step(s, ctx, "sweep-1", opts); err != nil { // -> needs_results_pull
		t.Fatal(err)
	}
	if _, err := step(s, ctx, "sweep-1", opts); err != nil { // -> needs_teardown
		t.Fatal(err)
	}

	_, err := step(s, ctx, "sweep-1", opts) // teardown + finalize -> budget exhausted
	if err == nil || !strings.Contains(err.Error(), "exhausted its failure budget") {
		t.Fatalf("err = %v", err)
	}

	sw, _ := st.GetSweep("sweep-1")
	if sw.ErrorAction != ActionBudgetExhausted {
		t.Errorf("sweep = %+v", sw)
	}

	// Calling Step again (as `dbarenactl resume` would) must keep refusing,
	// not silently start over -- there's nothing to retry here.
	launchesBefore := len(fb.launchCalls)
	_, err = step(s, ctx, "sweep-1", opts)
	if err == nil || !strings.Contains(err.Error(), "exhausted its failure budget") {
		t.Fatalf("second call err = %v", err)
	}
	if len(fb.launchCalls) != launchesBefore {
		t.Error("must not launch new attempts for an exhausted test point")
	}
}

// TestStep_SkippedTestPointExcludedFromSchedulingAndCompletion reproduces
// `dbarenactl resume`'s skip recovery choice at the scheduler level: a test
// point manually marked Skipped (via sweepstate.Store.SkipTestPoint) must
// never be launched again and must not block the sweep from completing once
// every other test point is satisfied.
func TestStep_SkippedTestPointExcludedFromSchedulingAndCompletion(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	sw := &sweepstate.Sweep{ID: "sweep-1", Provider: "AWS", ParamsJSON: "{}", CreatedAt: time.Now().UTC()}
	if err := st.CreateSweep(sw); err != nil {
		t.Fatal(err)
	}
	skipped := &sweepstate.TestPoint{
		ID: "sweep-1-skipped", SweepID: sw.ID, Tier: "small", Workload: "tpcc", Scenario: "x.yaml",
		BoundType: "io", SuccessesNeeded: 5, FailureBudget: 1,
	}
	other := &sweepstate.TestPoint{
		ID: "sweep-1-other", SweepID: sw.ID, Tier: "medium", Workload: "tpcc", Scenario: "y.yaml",
		BoundType: "io", SuccessesNeeded: 1, FailureBudget: 1,
	}
	if err := st.CreateTestPoints([]*sweepstate.TestPoint{skipped, other}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordError(sw.ID, ActionBudgetExhausted, skipped.ID, "test point exhausted"); err != nil {
		t.Fatal(err)
	}
	if err := st.SkipTestPoint(sw.ID, skipped.ID); err != nil {
		t.Fatal(err)
	}

	opts := defaultOpts()
	ctx := context.Background()

	if _, err := step(s, ctx, sw.ID, opts); err != nil {
		t.Fatal(err)
	}
	if len(fb.launchCalls) != 1 {
		t.Fatalf("launchCalls = %d, want exactly 1 (the skipped test point must never be launched)", len(fb.launchCalls))
	}
	runID := fb.launchCalls[0]
	fb.complete(runID, true)
	if _, err := step(s, ctx, sw.ID, opts); err != nil { // -> needs_results_pull
		t.Fatal(err)
	}
	if _, err := step(s, ctx, sw.ID, opts); err != nil { // -> needs_teardown
		t.Fatal(err)
	}
	result, err := step(s, ctx, sw.ID, opts) // teardown + finalize -> sweep complete
	if err != nil {
		t.Fatal(err)
	}
	if !result.Done {
		t.Errorf("sweep should complete once every non-skipped test point is satisfied, result = %+v", result)
	}

	sweepAfter, err := st.GetSweep(sw.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sweepAfter.Status != sweepstate.SweepCompleted {
		t.Errorf("sweep status = %q, want completed", sweepAfter.Status)
	}
}

// TestStep_SkippedTestPointStragglerKeepsSweepOpenUntilTornDown reproduces a
// real race: a test point can have more than one concurrent attempt in
// flight, so the very attempt that pushes it over its failure budget (making
// it eligible to skip) can leave a second, still-executing attempt for that
// same test point genuinely non-terminal. Marking the sweep complete while
// that straggler is still up would orphan its environment for good -- once a
// sweep is SweepCompleted, nothing ever calls Step for it again. Step must
// keep the sweep open until the straggler finishes reconciling (and is torn
// down) before ever declaring it done.
func TestStep_SkippedTestPointStragglerKeepsSweepOpenUntilTornDown(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	sw, flaky := seedSweepWithOneTestPoint(t, st, 2, 1) // 2 successes needed, only 1 failure tolerated
	opts := defaultOpts()
	ctx := context.Background()

	// Spare capacity launches two concurrent attempts for the one test
	// point (remaining successes needed caps it at exactly 2).
	if _, err := fill(t, s, fb, ctx, sw.ID, opts); err != nil {
		t.Fatal(err)
	}
	if len(fb.launchCalls) != 2 {
		t.Fatalf("launchCalls = %v, want 2 concurrent attempts", fb.launchCalls)
	}
	failing, straggler := fb.launchCalls[0], fb.launchCalls[1]

	// Fail the first attempt and walk it all the way through reconciliation
	// -- waiting_remote -> needs_results_pull -> needs_teardown -> torn down
	// + finalized -- which is what actually exhausts the budget. The second
	// attempt is never completed here: it stays a genuine straggler
	// throughout.
	fb.complete(failing, false)
	if _, err := step(s, ctx, sw.ID, opts); err != nil { // -> needs_results_pull
		t.Fatal(err)
	}
	if _, err := step(s, ctx, sw.ID, opts); err != nil { // -> needs_teardown
		t.Fatal(err)
	}
	_, err := step(s, ctx, sw.ID, opts) // teardown + finalize -> budget exhausted
	if err == nil || !strings.Contains(err.Error(), "exhausted its failure budget") {
		t.Fatalf("err = %v", err)
	}

	if err := st.SkipTestPoint(sw.ID, flaky.ID); err != nil {
		t.Fatalf("SkipTestPoint: %v", err)
	}

	// The straggler is still non-terminal (never completed) -- the sweep
	// must not be marked done yet, even though its only test point is now
	// skipped and therefore vacuously "allSatisfied".
	result, err := step(s, ctx, sw.ID, opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.Done {
		t.Fatal("sweep marked Done while the skipped test point's straggler run is still non-terminal")
	}
	sweepMidway, err := st.GetSweep(sw.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sweepMidway.Status == sweepstate.SweepCompleted {
		t.Fatal("sweep marked completed while the straggler is still up -- its environment would never be torn down again")
	}

	// Now let the straggler actually finish and walk it through the same
	// reconciliation chain.
	fb.complete(straggler, true)
	if _, err := step(s, ctx, sw.ID, opts); err != nil { // -> needs_results_pull
		t.Fatal(err)
	}
	if _, err := step(s, ctx, sw.ID, opts); err != nil { // -> needs_teardown
		t.Fatal(err)
	}
	result, err = step(s, ctx, sw.ID, opts) // teardown + finalize -> now truly done
	if err != nil {
		t.Fatal(err)
	}
	if !result.Done {
		t.Fatalf("result = %+v, want Done once the straggler is torn down", result)
	}
	if len(fb.teardownCalls) != 2 {
		t.Errorf("teardownCalls = %v, want both attempts torn down", fb.teardownCalls)
	}
	sweepAfter, err := st.GetSweep(sw.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sweepAfter.Status != sweepstate.SweepCompleted {
		t.Errorf("sweep status = %q, want completed", sweepAfter.Status)
	}
}

// TestRestartExhaustedSweep_TearsDownStragglerBeforeWiping reproduces a real
// race: the same Step call that finalizes the run pushing test point A over
// its failure budget can, in that same call, hand a *different*, still-
// healthy test point a brand-new launch (freed capacity + spare budget). By
// the time the sweep stops on A's exhaustion, that other run is genuinely
// non-terminal. RestartExhaustedSweep must tear it down before wiping any
// rows, or it would silently orphan whatever infra it represents.
func TestRestartExhaustedSweep_TearsDownStragglerBeforeWiping(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	sw := &sweepstate.Sweep{ID: "sweep-1", Provider: "AWS", ParamsJSON: "{}", CreatedAt: time.Now().UTC()}
	if err := st.CreateSweep(sw); err != nil {
		t.Fatal(err)
	}
	tpA := &sweepstate.TestPoint{ID: "sweep-1-a", SweepID: sw.ID, Tier: "small", Workload: "tpcc", Scenario: "x.yaml", BoundType: "io", SuccessesNeeded: 5, FailureBudget: 1}
	tpC := &sweepstate.TestPoint{ID: "sweep-1-c", SweepID: sw.ID, Tier: "small", Workload: "tpcc", Scenario: "x.yaml", BoundType: "compute", SuccessesNeeded: 2, FailureBudget: 5}
	if err := st.CreateTestPoints([]*sweepstate.TestPoint{tpA, tpC}); err != nil {
		t.Fatal(err)
	}
	opts := defaultOpts()
	opts.MaxConcurrency = 2
	ctx := context.Background()

	// Launch both.
	if _, err := fill(t, s, fb, ctx, "sweep-1", opts); err != nil {
		t.Fatal(err)
	}
	if len(fb.launchCalls) != 2 {
		t.Fatalf("launchCalls once filled = %v", fb.launchCalls)
	}
	runA, runC := fb.launchCalls[0], fb.launchCalls[1]
	fb.complete(runA, false) // A's only attempt fails
	fb.complete(runC, true)  // C's first attempt succeeds

	// Next two passes: advance both runs to needs_teardown. The pass after
	// finalizes both in the same call: A's failure trips its budget (1/1 failures, still
	// 0/5 successes) in the very same pass that finalizes C's success and
	// frees capacity + spare budget for C's second (of 2 needed) attempt --
	// reproducing the race.
	for i := 0; i < 2; i++ {
		if _, err := step(s, ctx, "sweep-1", opts); err != nil {
			t.Fatalf("drain pass %d: %v", i+1, err)
		}
	}
	_, err := step(s, ctx, "sweep-1", opts) // finalize + exhaust + launch stray
	if err == nil || !strings.Contains(err.Error(), "exhausted its failure budget") {
		t.Fatalf("final pass err = %v, want budget-exhausted error", err)
	}

	if len(fb.launchCalls) != 3 {
		t.Fatalf("launchCalls = %v, want a fresh launch for C's second attempt alongside the exhausted A", fb.launchCalls)
	}
	strayRunID := fb.launchCalls[2]
	strayRun, err := st.GetRun(strayRunID)
	if err != nil {
		t.Fatalf("stray run should exist: %v", err)
	}
	if strayRun.Status.Terminal() {
		t.Fatalf("stray run %s should be non-terminal at the moment of exhaustion, got %q", strayRunID, strayRun.Status)
	}

	if err := s.RestartExhaustedSweep(ctx, "sweep-1"); err != nil {
		t.Fatalf("RestartExhaustedSweep: %v", err)
	}

	found := false
	for _, id := range fb.teardownCalls {
		if id == strayRunID {
			found = true
		}
	}
	if !found {
		t.Errorf("teardownCalls = %v, want it to include the still non-terminal stray run %s", fb.teardownCalls, strayRunID)
	}

	if _, err := st.GetRun(strayRunID); !errors.Is(err, sweepstate.ErrNotFound) {
		t.Errorf("stray run should be deleted after restart, err = %v", err)
	}
	tps, err := st.ListTestPoints("sweep-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, tp := range tps {
		if tp.SuccessesCount != 0 || tp.FailuresCount != 0 {
			t.Errorf("tp %s after restart = %+v, want zeroed counters", tp.ID, tp)
		}
	}
	sweepAfter, err := st.GetSweep("sweep-1")
	if err != nil {
		t.Fatal(err)
	}
	if sweepAfter.Status != sweepstate.SweepRunning || sweepAfter.HasError() {
		t.Errorf("sweep after restart = %+v", sweepAfter)
	}
}

func TestRestartExhaustedSweep_RejectsNonBudgetExhaustedSweep(t *testing.T) {
	s, _, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 1, 1)
	if err := s.RestartExhaustedSweep(context.Background(), "sweep-1"); err == nil {
		t.Error("RestartExhaustedSweep on a healthy sweep should have errored")
	}
}

func TestStep_FetchExhaustsRetries_StopsTheWorld(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 1, 1)
	opts := defaultOpts()
	opts.FetchRetryLimit = 2
	ctx := context.Background()

	if _, err := step(s, ctx, "sweep-1", opts); err != nil {
		t.Fatal(err)
	}
	runID := fb.launchCalls[0]
	fb.complete(runID, true)
	if _, err := step(s, ctx, "sweep-1", opts); err != nil { // -> needs_results_pull
		t.Fatal(err)
	}
	fb.run(runID).fetchFailCount = 999 // always fails

	if _, err := step(s, ctx, "sweep-1", opts); err != nil { // attempt 1, under limit
		t.Fatal(err)
	}
	_, err := step(s, ctx, "sweep-1", opts) // attempt 2, hits limit
	if err == nil || !strings.Contains(err.Error(), "results-pull failed") {
		t.Fatalf("err = %v", err)
	}

	sw, _ := st.GetSweep("sweep-1")
	if sw.ErrorAction != ActionFetch {
		t.Errorf("sweep = %+v", sw)
	}
}

func TestStep_TeardownFailure_StopsTheWorld(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 1, 1)
	opts := defaultOpts()
	ctx := context.Background()

	if _, err := step(s, ctx, "sweep-1", opts); err != nil {
		t.Fatal(err)
	}
	runID := fb.launchCalls[0]
	fb.complete(runID, true)
	if _, err := step(s, ctx, "sweep-1", opts); err != nil { // -> needs_results_pull
		t.Fatal(err)
	}
	if _, err := step(s, ctx, "sweep-1", opts); err != nil { // -> needs_teardown
		t.Fatal(err)
	}
	fb.run(runID).teardownErr = errors.New("VPC deletion blocked: dependency violation")

	_, err := step(s, ctx, "sweep-1", opts)
	if err == nil || !strings.Contains(err.Error(), "dependency violation") {
		t.Fatalf("err = %v", err)
	}
	sw, _ := st.GetSweep("sweep-1")
	if sw.ErrorAction != ActionTeardown {
		t.Errorf("sweep = %+v", sw)
	}
	if out := s.Out.(*strings.Builder).String(); !strings.Contains(out, runID+": tearing down") {
		t.Errorf("output missing tearing-down line before the failed attempt: %q", out)
	}

	// Resuming with teardown now fixed should finalize and complete.
	fb.run(runID).teardownErr = nil
	res, err := step(s, ctx, "sweep-1", opts)
	if err != nil {
		t.Fatalf("retry step: %v", err)
	}
	if !res.Done {
		t.Errorf("res = %+v, want Done", res)
	}
}

// A run that reached needs_teardown (results already pulled) but whose
// environment vanished from benchctl's state store entirely -- e.g. someone
// followed docs/troubleshooting.md's advice and ran `benchctl teardown
// <run-id>` by hand after a previous teardown attempt failed -- must finalize
// on its own instead of retrying a teardown that can only ever fail again.
func TestStep_Teardown_RunVanished_FinalizesWithoutRetrying(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 1, 1)
	opts := defaultOpts()
	ctx := context.Background()

	if _, err := step(s, ctx, "sweep-1", opts); err != nil {
		t.Fatal(err)
	}
	runID := fb.launchCalls[0]
	fb.complete(runID, true)
	if _, err := step(s, ctx, "sweep-1", opts); err != nil { // -> needs_results_pull
		t.Fatal(err)
	}
	if _, err := step(s, ctx, "sweep-1", opts); err != nil { // -> needs_teardown
		t.Fatal(err)
	}
	fb.run(runID).notFound = true

	res, err := step(s, ctx, "sweep-1", opts)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if !res.Progressed || !res.Done {
		t.Errorf("res = %+v, want Progressed and Done", res)
	}
	if len(fb.teardownCalls) != 0 {
		t.Errorf("must not attempt to tear down a run benchctl has no record of, got %v", fb.teardownCalls)
	}
	run, _ := st.GetRun(runID)
	if run.Status != sweepstate.RunDone {
		t.Errorf("run.Status = %q, want %q", run.Status, sweepstate.RunDone)
	}
	if run.Outcome != "success" {
		t.Errorf("run.Outcome = %q, want success", run.Outcome)
	}
	sw, _ := st.GetSweep("sweep-1")
	if sw.HasError() {
		t.Errorf("a vanished-but-already-gone environment must not stop the sweep, got action %q", sw.ErrorAction)
	}
}

// Same as above, but benchctl still has a record of the run and confirms via
// TerminatedAt that the environment already came down, rather than having
// lost the record entirely.
func TestStep_Teardown_EnvironmentAlreadyTerminated_FinalizesWithoutRetrying(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 1, 1)
	opts := defaultOpts()
	ctx := context.Background()

	if _, err := step(s, ctx, "sweep-1", opts); err != nil {
		t.Fatal(err)
	}
	runID := fb.launchCalls[0]
	fb.complete(runID, true)
	if _, err := step(s, ctx, "sweep-1", opts); err != nil { // -> needs_results_pull
		t.Fatal(err)
	}
	if _, err := step(s, ctx, "sweep-1", opts); err != nil { // -> needs_teardown
		t.Fatal(err)
	}
	now := time.Now().UTC()
	fb.run(runID).state.TerminatedAt = &now

	res, err := step(s, ctx, "sweep-1", opts)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if !res.Progressed || !res.Done {
		t.Errorf("res = %+v, want Progressed and Done", res)
	}
	if len(fb.teardownCalls) != 0 {
		t.Errorf("must not tear down an already-terminated environment, got %v", fb.teardownCalls)
	}
	run, _ := st.GetRun(runID)
	if run.Status != sweepstate.RunDone {
		t.Errorf("run.Status = %q, want %q", run.Status, sweepstate.RunDone)
	}
	if run.Outcome != "success" {
		t.Errorf("run.Outcome = %q, want success", run.Outcome)
	}
	sw, _ := st.GetSweep("sweep-1")
	if sw.HasError() {
		t.Errorf("an already-terminated environment must not stop the sweep, got action %q", sw.ErrorAction)
	}
}

// staleRun drives a sweep to one launched run and backdates its heartbeat past
// StaleThreshold, which is the state every test below starts from.
func staleRun(t *testing.T, s *Scheduler, fb *fakeBench, opts Options, frozen time.Time) string {
	t.Helper()
	if _, err := step(s, context.Background(), "sweep-1", opts); err != nil {
		t.Fatal(err)
	}
	runID := fb.launchCalls[0]
	stale := frozen.Add(-20 * time.Minute)
	fb.run(runID).state.Phases[bench.PhaseWorkloadExecute] = "running"
	fb.run(runID).state.LastHeartbeat = &stale
	return runID
}

func TestStep_StaleHeartbeat_FetchesDiagnosticsThenTearsDown(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 1, 2)
	frozen := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return frozen }
	opts := defaultOpts()
	ctx := context.Background()
	runID := staleRun(t, s, fb, opts, frozen)

	res, err := step(s, ctx, "sweep-1", opts)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if !res.Progressed {
		t.Error("failing a stale run is progress")
	}
	if len(fb.fetchCalls) != 1 || fb.fetchCalls[0] != runID {
		t.Errorf("expected a best-effort diagnostics fetch, got %v", fb.fetchCalls)
	}
	run, _ := st.GetRun(runID)
	if run.Status != sweepstate.RunNeedsTeardown {
		t.Errorf("run.Status = %q, want %q", run.Status, sweepstate.RunNeedsTeardown)
	}
	if run.Outcome != "failure" {
		t.Errorf("run.Outcome = %q, want failure", run.Outcome)
	}
	// Recorded so the partial artifacts can be found, but "failure" keeps them
	// out of the results (see cmd/dbarenactl/results.go).
	if run.LocalArtifactDir == "" {
		t.Error("a successful diagnostics fetch should record its artifact dir")
	}

	// The next pass runs the teardown and charges the failure budget.
	if _, err := step(s, ctx, "sweep-1", opts); err != nil {
		t.Fatalf("teardown step: %v", err)
	}
	if len(fb.teardownCalls) != 1 || fb.teardownCalls[0] != runID {
		t.Errorf("teardownCalls = %v, want exactly %s", fb.teardownCalls, runID)
	}
	run, _ = st.GetRun(runID)
	if run.Status != sweepstate.RunFailed {
		t.Errorf("run.Status = %q, want %q", run.Status, sweepstate.RunFailed)
	}
	tp, _ := st.GetTestPoint(run.TestPointID)
	if tp.FailuresCount != 1 {
		t.Errorf("tp.FailuresCount = %d, want 1", tp.FailuresCount)
	}
}

// The ordinary dead-driver case: the instance is gone, so the diagnostics fetch
// cannot work. That must not stop the sweep -- the whole point of failing a
// stale run is that the sweep keeps making progress.
func TestStep_StaleHeartbeat_FetchFailureStillTearsDown(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 1, 2)
	frozen := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return frozen }
	opts := defaultOpts()
	ctx := context.Background()
	runID := staleRun(t, s, fb, opts, frozen)
	fb.run(runID).fetchErr = errors.New("ssh: connect to host 1.2.3.4 port 22: No route to host")

	res, err := step(s, ctx, "sweep-1", opts)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if !res.Progressed {
		t.Error("failing a stale run is progress even when diagnostics are lost")
	}
	run, _ := st.GetRun(runID)
	if run.Status != sweepstate.RunNeedsTeardown {
		t.Errorf("run.Status = %q, want %q", run.Status, sweepstate.RunNeedsTeardown)
	}
	if run.Outcome != "failure" {
		t.Errorf("run.Outcome = %q, want failure", run.Outcome)
	}
	if run.LocalArtifactDir != "" {
		t.Errorf("a failed fetch must not record an artifact dir, got %q", run.LocalArtifactDir)
	}
	sw, _ := st.GetSweep("sweep-1")
	if sw.HasError() {
		t.Errorf("a failed diagnostics fetch must not stop the sweep, got action %q", sw.ErrorAction)
	}
}

// A stale run whose environment is already gone must take the
// TerminatedWithoutCompleting path: tearing down torn-down infrastructure would
// fail, and a teardown failure stops the whole sweep.
func TestStep_StaleHeartbeat_AlreadyTerminated_SkipsTeardown(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 1, 2)
	frozen := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return frozen }
	opts := defaultOpts()
	ctx := context.Background()
	runID := staleRun(t, s, fb, opts, frozen)
	terminated := frozen.Add(-5 * time.Minute)
	fb.run(runID).state.TerminatedAt = &terminated

	if _, err := step(s, ctx, "sweep-1", opts); err != nil {
		t.Fatalf("Step: %v", err)
	}
	if len(fb.teardownCalls) != 0 {
		t.Errorf("must not tear down an already-terminated environment, got %v", fb.teardownCalls)
	}
	if len(fb.fetchCalls) != 0 {
		t.Errorf("must not fetch from an already-terminated environment, got %v", fb.fetchCalls)
	}
	run, _ := st.GetRun(runID)
	if run.Status != sweepstate.RunFailed {
		t.Errorf("run.Status = %q, want %q", run.Status, sweepstate.RunFailed)
	}
}

func TestStep_WaitingRemoteTerminatedWithoutCompleting_FinalizesAsFailure(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	// Failure budget wider than 1 so finalizing this one failure doesn't
	// itself exhaust it -- this test is about the reconciliation branch
	// itself, not the separate budget-exhausted stop-the-world path.
	_, tp := seedSweepWithOneTestPoint(t, st, 1, 2)

	if _, err := step(s, context.Background(), "sweep-1", defaultOpts()); err != nil {
		t.Fatal(err)
	}
	runID := fb.launchCalls[0]

	// Simulate a manual `benchctl teardown <run-id>` on a run stuck for
	// good: the environment comes down, but the workload never reported a
	// result.
	now := time.Now().UTC()
	fb.run(runID).state.TerminatedAt = &now

	res, err := step(s, context.Background(), "sweep-1", defaultOpts())
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if !res.Progressed {
		t.Error("finalizing the run should count as progress")
	}
	if len(fb.fetchCalls) != 0 || len(fb.teardownCalls) != 0 {
		t.Errorf("should neither fetch nor tear down an already-terminated environment: fetch=%v teardown=%v", fb.fetchCalls, fb.teardownCalls)
	}
	run, err := st.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != sweepstate.RunFailed {
		t.Errorf("run.Status = %q, want %q", run.Status, sweepstate.RunFailed)
	}
	if run.Outcome != "failure" {
		t.Errorf("run.Outcome = %q, want %q", run.Outcome, "failure")
	}
	tpAfter, err := st.GetTestPoint(tp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tpAfter.FailuresCount != 1 {
		t.Errorf("FailuresCount = %d, want 1", tpAfter.FailuresCount)
	}
}

func TestStep_NeedsResultsPullTerminated_FinalizesAsFailure(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	// Failure budget wider than 1 so finalizing this one failure doesn't
	// itself exhaust it -- this test is about the reconciliation branch
	// itself, not the separate budget-exhausted stop-the-world path.
	_, tp := seedSweepWithOneTestPoint(t, st, 1, 2)

	if _, err := step(s, context.Background(), "sweep-1", defaultOpts()); err != nil {
		t.Fatal(err)
	}
	runID := fb.launchCalls[0]
	fb.complete(runID, true)
	if _, err := step(s, context.Background(), "sweep-1", defaultOpts()); err != nil { // -> needs_results_pull
		t.Fatal(err)
	}

	// Simulate a manual `benchctl teardown <run-id>` on the environment
	// after the workload completed but before results were fetched.
	now := time.Now().UTC()
	fb.run(runID).state.TerminatedAt = &now

	res, err := step(s, context.Background(), "sweep-1", defaultOpts())
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if !res.Progressed {
		t.Error("finalizing the run should count as progress")
	}
	if len(fb.fetchCalls) != 0 || len(fb.teardownCalls) != 0 {
		t.Errorf("should neither fetch nor tear down an already-terminated environment: fetch=%v teardown=%v", fb.fetchCalls, fb.teardownCalls)
	}
	run, err := st.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != sweepstate.RunFailed {
		t.Errorf("run.Status = %q, want %q", run.Status, sweepstate.RunFailed)
	}
	if run.Outcome != "failure" {
		t.Errorf("run.Outcome = %q, want %q", run.Outcome, "failure")
	}
	tpAfter, err := st.GetTestPoint(tp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tpAfter.FailuresCount != 1 {
		t.Errorf("FailuresCount = %d, want 1", tpAfter.FailuresCount)
	}
}

func TestStep_RespectsMaxConcurrency(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	sw := &sweepstate.Sweep{ID: "sweep-1", Provider: "AWS", ParamsJSON: "{}", CreatedAt: time.Now().UTC()}
	if err := st.CreateSweep(sw); err != nil {
		t.Fatal(err)
	}
	var tps []*sweepstate.TestPoint
	for _, tier := range []string{"small", "medium", "large"} {
		tps = append(tps, &sweepstate.TestPoint{
			ID: "sweep-1-" + tier, SweepID: sw.ID, Tier: tier, Workload: "tpcc", Scenario: "x.yaml",
			BoundType: "io", SuccessesNeeded: 1, FailureBudget: 1,
		})
	}
	if err := st.CreateTestPoints(tps); err != nil {
		t.Fatal(err)
	}

	opts := defaultOpts()
	opts.MaxConcurrency = 2
	res, err := fill(t, s, fb, context.Background(), "sweep-1", opts)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Progressed {
		t.Error("expected progress")
	}
	if len(fb.launchCalls) != 2 {
		t.Fatalf("launchCalls = %v, want 2 (max-concurrency respected)", fb.launchCalls)
	}

	// Finish one of them; the third test point should now get a slot.
	fb.complete(fb.launchCalls[0], true)
	if _, err := step(s, context.Background(), "sweep-1", opts); err != nil { // -> needs_results_pull
		t.Fatal(err)
	}
	if _, err := step(s, context.Background(), "sweep-1", opts); err != nil { // -> needs_teardown
		t.Fatal(err)
	}
	if _, err := step(s, context.Background(), "sweep-1", opts); err != nil { // finalize + launch #3
		t.Fatal(err)
	}
	if len(fb.launchCalls) != 3 {
		t.Fatalf("launchCalls = %v, want 3 once a slot freed up", fb.launchCalls)
	}
}

// TestStep_StacksConcurrentAttemptsBreadthFirst verifies that spare
// concurrency spreads one attempt per test point per round before it stacks
// a second attempt on any one of them -- so a test point needing several
// successes never starves its neighbors of their first attempt.
func TestStep_StacksConcurrentAttemptsBreadthFirst(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	sw := &sweepstate.Sweep{ID: "sweep-1", Provider: "AWS", ParamsJSON: "{}", CreatedAt: time.Now().UTC()}
	if err := st.CreateSweep(sw); err != nil {
		t.Fatal(err)
	}
	tpA := &sweepstate.TestPoint{ID: "sweep-1-a", SweepID: sw.ID, Tier: "small", Workload: "tpcc", Scenario: "x.yaml", BoundType: "io", SuccessesNeeded: 3, FailureBudget: 1}
	tpB := &sweepstate.TestPoint{ID: "sweep-1-b", SweepID: sw.ID, Tier: "medium", Workload: "tpcc", Scenario: "x.yaml", BoundType: "io", SuccessesNeeded: 3, FailureBudget: 1}
	if err := st.CreateTestPoints([]*sweepstate.TestPoint{tpA, tpB}); err != nil {
		t.Fatal(err)
	}

	opts := defaultOpts()
	opts.MaxConcurrency = 3
	if _, err := fill(t, s, fb, context.Background(), "sweep-1", opts); err != nil {
		t.Fatal(err)
	}

	// 3 slots, 2 test points each wanting up to 3: round-robin gives A and B
	// one each, then stacks the third slot on A (first in iteration order)
	// rather than either starving B of its first attempt or maxing out A
	// before B gets a look-in.
	if len(fb.launchCalls) != 3 {
		t.Fatalf("launchCalls = %v, want 3", fb.launchCalls)
	}
	countByTestPoint := map[string]int{}
	for _, runID := range fb.launchCalls {
		run, err := st.GetRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		countByTestPoint[run.TestPointID]++
	}
	if countByTestPoint["sweep-1-a"] != 2 || countByTestPoint["sweep-1-b"] != 1 {
		t.Fatalf("countByTestPoint = %v, want A=2 (got the extra slot) and B=1 (still got its first attempt)", countByTestPoint)
	}
}

// TestStep_BreadthFirstAcrossPasses verifies that breadth-first spreading
// holds across scheduling passes and completions, not just within one pass's
// concurrency budget. At max-concurrency 1 -- the production default -- only
// one run is ever in flight, so the only way to see round-robin behavior is
// by tracking attempts already completed, not just attempts in flight.
func TestStep_BreadthFirstAcrossPasses(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	sw := &sweepstate.Sweep{ID: "sweep-1", Provider: "AWS", ParamsJSON: "{}", CreatedAt: time.Now().UTC()}
	if err := st.CreateSweep(sw); err != nil {
		t.Fatal(err)
	}
	var tps []*sweepstate.TestPoint
	for _, tier := range []string{"small", "medium", "large"} {
		tps = append(tps, &sweepstate.TestPoint{
			ID: "sweep-1-" + tier, SweepID: sw.ID, Tier: tier, Workload: "tpcc", Scenario: "x.yaml",
			BoundType: "io", SuccessesNeeded: 2, FailureBudget: 1,
		})
	}
	if err := st.CreateTestPoints(tps); err != nil {
		t.Fatal(err)
	}

	opts := defaultOpts()
	opts.MaxConcurrency = 1
	ctx := context.Background()

	var order []string
	for i := 0; i < 6; i++ {
		if _, err := fill(t, s, fb, ctx, "sweep-1", opts); err != nil {
			t.Fatal(err)
		}
		if len(fb.launchCalls) != i+1 {
			t.Fatalf("after round %d: launchCalls = %v, want %d", i, fb.launchCalls, i+1)
		}
		runID := fb.launchCalls[i]
		run, err := st.GetRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		order = append(order, run.TestPointID)

		fb.complete(runID, true)
		if _, err := step(s, ctx, "sweep-1", opts); err != nil { // -> needs_results_pull
			t.Fatal(err)
		}
		if _, err := step(s, ctx, "sweep-1", opts); err != nil { // -> needs_teardown
			t.Fatal(err)
		}
		if _, err := step(s, ctx, "sweep-1", opts); err != nil { // finalize (+ next launch)
			t.Fatal(err)
		}
	}

	// Every test point must get its first attempt before any gets a second:
	// the first 3 launches are 3 distinct test points, and the next 3 repeat
	// them in the same order for their second attempt.
	first := order[:3]
	seen := map[string]bool{}
	for _, id := range first {
		if seen[id] {
			t.Fatalf("order = %v: a test point got a second attempt before every test point had a first", order)
		}
		seen[id] = true
	}
	for i, id := range order[3:6] {
		if id != first[i] {
			t.Fatalf("order = %v: want second round to repeat first round's order", order)
		}
	}
}

func TestRunSweep_CompletesAndReturnsNil(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 1, 1)
	opts := defaultOpts()
	opts.PollInterval = time.Millisecond

	var workloads sync.WaitGroup
	fb.onLaunch = func(runID string) {
		workloads.Add(1)
		go func() {
			defer workloads.Done()
			// Simulate the workload finishing shortly after launch.
			time.Sleep(2 * time.Millisecond)
			fb.complete(runID, true)
		}()
	}
	defer workloads.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.RunSweep(ctx, "sweep-1", opts); err != nil {
		t.Fatalf("RunSweep: %v", err)
	}
	sw, _ := st.GetSweep("sweep-1")
	if sw.Status != sweepstate.SweepCompleted {
		t.Errorf("sweep status = %q", sw.Status)
	}
}

func TestRunSweep_StopsOnContextCancellation(t *testing.T) {
	s, _, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 1, 1)
	opts := defaultOpts()
	opts.PollInterval = time.Hour // never resolves on its own within the test

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.RunSweep(ctx, "sweep-1", opts)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestClassifyLaunching(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-1 * time.Minute)
	old := now.Add(-10 * time.Minute)

	cases := []struct {
		name      string
		phases    map[string]string
		createdAt time.Time
		want      launchingVerdict
	}{
		{"still provisioning", map[string]string{}, recent, verdictOrphaned},
		{"provision completed, recent, nothing later", map[string]string{"provision": "completed"}, recent, verdictAmbiguous},
		{"provision completed, old, nothing later", map[string]string{"provision": "completed"}, old, verdictOrphaned},
		{"driver.setup running", map[string]string{"provision": "completed", "driver.setup": "running"}, recent, verdictHandedOff},
		{"driver.setup running despite old createdAt", map[string]string{"provision": "completed", "driver.setup": "running"}, old, verdictHandedOff},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := &bench.RunState{Phases: tc.phases}
			if got := classifyLaunching(rs, tc.createdAt, now); got != tc.want {
				t.Errorf("classifyLaunching() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRunSweep_DrainsFinishedRunsWhileALaunchIsInFlight is the regression
// test for the sweep that left eight finished environments billing for up to
// 87 minutes each: the control loop filled every concurrency slot in one
// pass, blocking on each provision in turn, so nothing was fetched or torn
// down until the last one returned. It drives RunSweep rather than Step
// directly -- calling Step from the test would supply exactly the
// reconciliation the bug withheld.
func TestRunSweep_DrainsFinishedRunsWhileALaunchIsInFlight(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	sw := &sweepstate.Sweep{ID: "sweep-1", Provider: "AWS", ParamsJSON: "{}", CreatedAt: time.Now().UTC()}
	if err := st.CreateSweep(sw); err != nil {
		t.Fatal(err)
	}
	tpA := &sweepstate.TestPoint{ID: "sweep-1-a", SweepID: sw.ID, Tier: "small", Workload: "tpcc", Scenario: "x.yaml", BoundType: "io", SuccessesNeeded: 1, FailureBudget: 1}
	tpB := &sweepstate.TestPoint{ID: "sweep-1-b", SweepID: sw.ID, Tier: "medium", Workload: "tpcc", Scenario: "x.yaml", BoundType: "io", SuccessesNeeded: 1, FailureBudget: 1}
	if err := st.CreateTestPoints([]*sweepstate.TestPoint{tpA, tpB}); err != nil {
		t.Fatal(err)
	}
	opts := defaultOpts()

	// A hands off immediately. B's launch finishes A's workload and then
	// parks, standing in for a provision that runs for tens of minutes.
	var launches int
	blocked, release := make(chan struct{}), make(chan struct{})
	var first string
	fb.onLaunch = func(runID string) {
		launches++
		if launches == 1 {
			first = runID
			return
		}
		fb.complete(first, true)
		close(blocked)
		<-release
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.RunSweep(ctx, "sweep-1", opts) }()
	defer func() {
		cancel()
		close(release)
		<-done
	}()

	<-blocked // a provision is now in flight and stays there

	// A must reach done -- fetched and torn down -- without waiting for that
	// provision to return.
	deadline := time.Now().Add(5 * time.Second)
	for {
		run, err := st.GetRun(first)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == sweepstate.RunDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s stuck at %q with a launch in flight -- a finished environment must drain during a provision, not after it", first, run.Status)
		}
		time.Sleep(time.Millisecond)
	}

	if !s.launchBusy() {
		t.Fatal("the launch should still be in flight -- the test proves nothing otherwise")
	}
	if !slices.Contains(fb.fetchCalls, first) {
		t.Errorf("fetchCalls = %v, want %s fetched during the launch", fb.fetchCalls, first)
	}
	if !slices.Contains(fb.teardownCalls, first) {
		t.Errorf("teardownCalls = %v, want %s torn down during the launch", fb.teardownCalls, first)
	}
}

// TestStep_LaunchInFlightIsNotTreatedAsOrphan guards the interaction between
// the async launcher and the orphan detection: a provision this process is
// still running reports only "provision: running", which classifyLaunching
// would otherwise read as a crashed attempt and tear down underneath itself.
func TestStep_LaunchInFlightIsNotTreatedAsOrphan(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 1, 1)
	opts := defaultOpts()
	ctx := context.Background()

	blocked, release := make(chan struct{}), make(chan struct{})
	fb.onLaunch = func(runID string) {
		fb.setPhase(runID, bench.PhaseProvision, "running")
		close(blocked)
		<-release
	}
	defer s.WaitForLaunches()
	defer close(release)

	go s.Step(ctx, "sweep-1", opts) //nolint:errcheck
	<-blocked

	// Age the row well past the handoff grace period, which is what tips an
	// genuinely orphaned run over into teardown.
	s.Now = func() time.Time { return time.Now().Add(10 * bench.HandoffGracePeriod) }

	if _, err := s.Step(ctx, "sweep-1", opts); err != nil {
		t.Fatalf("Step: %v", err)
	}
	if len(fb.teardownCalls) != 0 {
		t.Errorf("teardownCalls = %v, want none -- the provision is still running here", fb.teardownCalls)
	}
	run, err := st.GetRun(fb.launchCalls[0])
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != sweepstate.RunLaunching {
		t.Errorf("run.Status = %q, want still launching", run.Status)
	}
}

// TestStep_DoesNotLaunchWhileARunAwaitsDraining pins the cost invariant: a
// finished environment is torn down before another one is ever started.
func TestStep_DoesNotLaunchWhileARunAwaitsDraining(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	sw := &sweepstate.Sweep{ID: "sweep-1", Provider: "AWS", ParamsJSON: "{}", CreatedAt: time.Now().UTC()}
	if err := st.CreateSweep(sw); err != nil {
		t.Fatal(err)
	}
	tpA := &sweepstate.TestPoint{ID: "sweep-1-a", SweepID: sw.ID, Tier: "small", Workload: "tpcc", Scenario: "x.yaml", BoundType: "io", SuccessesNeeded: 1, FailureBudget: 1}
	tpB := &sweepstate.TestPoint{ID: "sweep-1-b", SweepID: sw.ID, Tier: "medium", Workload: "tpcc", Scenario: "x.yaml", BoundType: "io", SuccessesNeeded: 1, FailureBudget: 1}
	if err := st.CreateTestPoints([]*sweepstate.TestPoint{tpA, tpB}); err != nil {
		t.Fatal(err)
	}
	opts := defaultOpts()
	ctx := context.Background()

	if _, err := step(s, ctx, "sweep-1", opts); err != nil {
		t.Fatal(err)
	}
	runA := fb.launchCalls[0]
	fb.complete(runA, true)
	// One transient fetch failure keeps A parked in needs_results_pull.
	fb.run(runA).fetchFailCount = 1

	if _, err := step(s, ctx, "sweep-1", opts); err != nil { // -> needs_results_pull
		t.Fatal(err)
	}
	launchesBefore := len(fb.launchCalls)
	if _, err := step(s, ctx, "sweep-1", opts); err != nil { // fetch fails, A still draining
		t.Fatal(err)
	}

	run, err := st.GetRun(runA)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != sweepstate.RunNeedsResultsPull {
		t.Fatalf("run.Status = %q, want needs_results_pull -- the test needs A still draining", run.Status)
	}
	if len(fb.launchCalls) != launchesBefore {
		t.Errorf("launched %v with a finished environment still awaiting teardown", fb.launchCalls[launchesBefore:])
	}
}

func TestStep_AsyncLaunchFailureStopsTheWorldOnNextPass(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 1, 1)
	opts := defaultOpts()
	ctx := context.Background()

	fb.onLaunch = func(runID string) { fb.run(runID).launchErr = errors.New("quota exceeded") }

	res, err := step(s, ctx, "sweep-1", opts)
	if err != nil {
		t.Fatalf("the pass that starts a launch must not block on its outcome: %v", err)
	}
	if !res.Progressed {
		t.Error("starting a launch counts as progress")
	}
	sw, _ := st.GetSweep("sweep-1")
	if sw.HasError() {
		t.Errorf("sweep error recorded too early: %+v", sw)
	}

	_, err = step(s, ctx, "sweep-1", opts)
	if err == nil || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("err = %v, want the parked launch failure", err)
	}
	sw, _ = st.GetSweep("sweep-1")
	if sw.ErrorAction != ActionLaunch || sw.ErrorTarget != fb.launchCalls[0] {
		t.Errorf("sweep = %+v, want a launch error against %s", sw, fb.launchCalls[0])
	}
}

// TestRunSweep_CancelsAndWaitsForInFlightLaunchOnReturn: returning must not
// leave a provision running unsupervised, and must not sit through a tofu
// apply that can take the better part of an hour either.
func TestRunSweep_CancelsAndWaitsForInFlightLaunchOnReturn(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 1, 1)
	opts := defaultOpts()

	launchCtxDone := make(chan struct{})
	blocked := make(chan struct{})
	fb.onLaunchCtx = func(ctx context.Context, _ string) {
		close(blocked)
		<-ctx.Done()
		close(launchCtxDone)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.RunSweep(ctx, "sweep-1", opts) }()

	<-blocked
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunSweep did not return after cancellation")
	}
	select {
	case <-launchCtxDone:
	case <-time.After(time.Second):
		t.Fatal("the in-flight launch was never cancelled")
	}
	if s.launchBusy() {
		t.Error("RunSweep returned with a launch goroutine still running")
	}
}

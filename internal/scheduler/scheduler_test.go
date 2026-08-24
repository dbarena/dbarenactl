package scheduler

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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
	sw := &sweepstate.Sweep{ID: "sweep-1", Provider: "aws/rds", ParamsJSON: "{}", CreatedAt: time.Now().UTC()}
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

func defaultOpts() Options {
	return Options{
		MaxConcurrency:  6,
		FetchRetryLimit: 3,
		ArtifactBaseDir: "/tmp/dbarenactl-test-artifacts",
		PollInterval:    time.Millisecond,
	}
}

// completeRun advances a fakeRun all the way to a finished (successful or
// failed) workload, as if benchctl had run it to completion.
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

	res, err := s.Step(context.Background(), "sweep-1", defaultOpts())
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
	if _, err := s.Step(ctx, "sweep-1", opts); err != nil {
		t.Fatalf("launch step: %v", err)
	}
	runID := fb.launchCalls[0]

	// Pass 2: still executing -- no progress expected.
	res, err := s.Step(ctx, "sweep-1", opts)
	if err != nil {
		t.Fatalf("waiting step: %v", err)
	}
	if res.Progressed || res.Done {
		t.Errorf("res while still executing = %+v", res)
	}

	// Workload finishes successfully.
	completeRun(fb.run(runID), true)
	res, err = s.Step(ctx, "sweep-1", opts)
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
	res, err = s.Step(ctx, "sweep-1", opts)
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
	res, err = s.Step(ctx, "sweep-1", opts)
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

	if _, err := s.Step(ctx, "sweep-1", opts); err != nil {
		t.Fatalf("launch step: %v", err)
	}
	runID := fb.launchCalls[0]
	if !strings.Contains(out.String(), "launching sweep-1-small-io") {
		t.Errorf("output missing launch line: %q", out.String())
	}
	if !strings.Contains(out.String(), runID+": provisioned") {
		t.Errorf("output missing provisioned line: %q", out.String())
	}

	completeRun(fb.run(runID), true)
	if _, err := s.Step(ctx, "sweep-1", opts); err != nil {
		t.Fatalf("completion step: %v", err)
	}
	if !strings.Contains(out.String(), runID+": workload finished (success)") {
		t.Errorf("output missing finished line: %q", out.String())
	}

	if _, err := s.Step(ctx, "sweep-1", opts); err != nil { // fetch
		t.Fatalf("fetch step: %v", err)
	}
	if _, err := s.Step(ctx, "sweep-1", opts); err != nil { // teardown + finalize
		t.Fatalf("teardown step: %v", err)
	}
	if !strings.Contains(out.String(), runID+": torn down -- sweep-1-small-io now 1/1 successes, 0/1 failures") {
		t.Errorf("output missing teardown line: %q", out.String())
	}
}

func TestStep_LaunchFailureStopsTheWorld(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 1, 1)

	fb.onLaunch = func(runID string) { fb.run(runID).launchErr = errors.New("AWS credentials expired") }

	_, err := s.Step(context.Background(), "sweep-1", defaultOpts())
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

	_, err := s.Step(context.Background(), "sweep-1", defaultOpts())
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

	_, err := s.Step(context.Background(), "sweep-1", defaultOpts())
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

	_, err := s.Step(context.Background(), "sweep-1", defaultOpts())
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

	res, err := s.Step(context.Background(), "sweep-1", defaultOpts())
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

	res, err := s.Step(context.Background(), "sweep-1", defaultOpts())
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

	res, err := s.Step(context.Background(), "sweep-1", defaultOpts())
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

	res, err := s.Step(context.Background(), "sweep-1", defaultOpts())
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
	// concurrently on the same test point from the very first pass.
	if _, err := s.Step(ctx, "sweep-1", opts); err != nil {
		t.Fatal(err)
	}
	if len(fb.launchCalls) != 2 {
		t.Fatalf("launchCalls after first pass = %v, want 2 (both needed successes stacked concurrently)", fb.launchCalls)
	}
	firstRunID := fb.launchCalls[0]
	completeRun(fb.run(firstRunID), false) // first attempt fails; second still in flight

	if _, err := s.Step(ctx, "sweep-1", opts); err != nil { // -> needs_results_pull
		t.Fatal(err)
	}
	if _, err := s.Step(ctx, "sweep-1", opts); err != nil { // -> needs_teardown (fetch)
		t.Fatal(err)
	}
	res, err := s.Step(ctx, "sweep-1", opts) // teardown + finalize as failure
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

	if _, err := s.Step(ctx, "sweep-1", opts); err != nil {
		t.Fatal(err)
	}
	runID := fb.launchCalls[0]
	completeRun(fb.run(runID), false)
	if _, err := s.Step(ctx, "sweep-1", opts); err != nil { // -> needs_results_pull
		t.Fatal(err)
	}
	if _, err := s.Step(ctx, "sweep-1", opts); err != nil { // -> needs_teardown
		t.Fatal(err)
	}

	_, err := s.Step(ctx, "sweep-1", opts) // teardown + finalize -> budget exhausted
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
	_, err = s.Step(ctx, "sweep-1", opts)
	if err == nil || !strings.Contains(err.Error(), "exhausted its failure budget") {
		t.Fatalf("second call err = %v", err)
	}
	if len(fb.launchCalls) != launchesBefore {
		t.Error("must not launch new attempts for an exhausted test point")
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
	sw := &sweepstate.Sweep{ID: "sweep-1", Provider: "aws/rds", ParamsJSON: "{}", CreatedAt: time.Now().UTC()}
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

	// Pass 1: launch both.
	if _, err := s.Step(ctx, "sweep-1", opts); err != nil {
		t.Fatal(err)
	}
	if len(fb.launchCalls) != 2 {
		t.Fatalf("launchCalls after pass 1 = %v", fb.launchCalls)
	}
	runA, runC := fb.launchCalls[0], fb.launchCalls[1]
	completeRun(fb.run(runA), false) // A's only attempt fails
	completeRun(fb.run(runC), true)  // C's first attempt succeeds

	// Passes 2-3: advance both runs to needs_teardown. Pass 4 finalizes both
	// in the same call: A's failure trips its budget (1/1 failures, still
	// 0/5 successes) in the very same pass that finalizes C's success and
	// frees capacity + spare budget for C's second (of 2 needed) attempt --
	// reproducing the race.
	for i := 0; i < 2; i++ {
		if _, err := s.Step(ctx, "sweep-1", opts); err != nil {
			t.Fatalf("pass %d: %v", i+2, err)
		}
	}
	_, err := s.Step(ctx, "sweep-1", opts) // pass 4: finalize + exhaust + launch stray
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

	if _, err := s.Step(ctx, "sweep-1", opts); err != nil {
		t.Fatal(err)
	}
	runID := fb.launchCalls[0]
	completeRun(fb.run(runID), true)
	if _, err := s.Step(ctx, "sweep-1", opts); err != nil { // -> needs_results_pull
		t.Fatal(err)
	}
	fb.run(runID).fetchFailCount = 999 // always fails

	if _, err := s.Step(ctx, "sweep-1", opts); err != nil { // attempt 1, under limit
		t.Fatal(err)
	}
	_, err := s.Step(ctx, "sweep-1", opts) // attempt 2, hits limit
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

	if _, err := s.Step(ctx, "sweep-1", opts); err != nil {
		t.Fatal(err)
	}
	runID := fb.launchCalls[0]
	completeRun(fb.run(runID), true)
	if _, err := s.Step(ctx, "sweep-1", opts); err != nil { // -> needs_results_pull
		t.Fatal(err)
	}
	if _, err := s.Step(ctx, "sweep-1", opts); err != nil { // -> needs_teardown
		t.Fatal(err)
	}
	fb.run(runID).teardownErr = errors.New("VPC deletion blocked: dependency violation")

	_, err := s.Step(ctx, "sweep-1", opts)
	if err == nil || !strings.Contains(err.Error(), "dependency violation") {
		t.Fatalf("err = %v", err)
	}
	sw, _ := st.GetSweep("sweep-1")
	if sw.ErrorAction != ActionTeardown {
		t.Errorf("sweep = %+v", sw)
	}

	// Resuming with teardown now fixed should finalize and complete.
	fb.run(runID).teardownErr = nil
	res, err := s.Step(ctx, "sweep-1", opts)
	if err != nil {
		t.Fatalf("retry step: %v", err)
	}
	if !res.Done {
		t.Errorf("res = %+v, want Done", res)
	}
}

func TestStep_StaleHeartbeat_WarnsButDoesNotTearDown(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 1, 1)
	frozen := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return frozen }
	var out strings.Builder
	s.Out = &out
	opts := defaultOpts()
	ctx := context.Background()

	if _, err := s.Step(ctx, "sweep-1", opts); err != nil {
		t.Fatal(err)
	}
	runID := fb.launchCalls[0]
	stale := frozen.Add(-20 * time.Minute)
	fb.run(runID).state.Phases[bench.PhaseWorkloadExecute] = "running"
	fb.run(runID).state.LastHeartbeat = &stale

	res, err := s.Step(ctx, "sweep-1", opts)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if len(fb.teardownCalls) != 0 {
		t.Error("a stale heartbeat must never trigger an automatic teardown")
	}
	if res.Progressed {
		t.Error("a stale warning alone is not progress")
	}
	if !strings.Contains(out.String(), "stale") {
		t.Errorf("expected a stale warning in output, got: %s", out.String())
	}
	run, _ := st.GetRun(runID)
	if run.Status != sweepstate.RunWaitingRemote {
		t.Errorf("run.Status = %q, should remain waiting_remote", run.Status)
	}
}

func TestStep_WaitingRemoteTerminatedWithoutCompleting_FinalizesAsFailure(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	// Failure budget wider than 1 so finalizing this one failure doesn't
	// itself exhaust it -- this test is about the reconciliation branch
	// itself, not the separate budget-exhausted stop-the-world path.
	_, tp := seedSweepWithOneTestPoint(t, st, 1, 2)

	if _, err := s.Step(context.Background(), "sweep-1", defaultOpts()); err != nil {
		t.Fatal(err)
	}
	runID := fb.launchCalls[0]

	// Simulate a manual `benchctl teardown <run-id>` on a run stuck for
	// good: the environment comes down, but the workload never reported a
	// result.
	now := time.Now().UTC()
	fb.run(runID).state.TerminatedAt = &now

	res, err := s.Step(context.Background(), "sweep-1", defaultOpts())
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
	sw := &sweepstate.Sweep{ID: "sweep-1", Provider: "aws/rds", ParamsJSON: "{}", CreatedAt: time.Now().UTC()}
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
	res, err := s.Step(context.Background(), "sweep-1", opts)
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
	completeRun(fb.run(fb.launchCalls[0]), true)
	if _, err := s.Step(context.Background(), "sweep-1", opts); err != nil { // -> needs_results_pull
		t.Fatal(err)
	}
	if _, err := s.Step(context.Background(), "sweep-1", opts); err != nil { // -> needs_teardown
		t.Fatal(err)
	}
	if _, err := s.Step(context.Background(), "sweep-1", opts); err != nil { // finalize + launch #3
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
	sw := &sweepstate.Sweep{ID: "sweep-1", Provider: "aws/rds", ParamsJSON: "{}", CreatedAt: time.Now().UTC()}
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
	if _, err := s.Step(context.Background(), "sweep-1", opts); err != nil {
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

func TestRunSweep_CompletesAndReturnsNil(t *testing.T) {
	s, fb, st := newTestScheduler(t)
	seedSweepWithOneTestPoint(t, st, 1, 1)
	opts := defaultOpts()
	opts.PollInterval = time.Millisecond

	fb.onLaunch = func(runID string) {
		go func() {
			// Simulate the workload finishing shortly after launch.
			time.Sleep(2 * time.Millisecond)
			completeRun(fb.run(runID), true)
		}()
	}

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

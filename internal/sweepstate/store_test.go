package sweepstate

import (
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "dbarenactl.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func seedSweep(t *testing.T, st *Store, id string) *Sweep {
	t.Helper()
	sw := &Sweep{ID: id, Provider: "AWS", Product: "RDS", Workload: "tpcc", ParamsJSON: "{}", CreatedAt: time.Now().UTC()}
	if err := st.CreateSweep(sw); err != nil {
		t.Fatalf("CreateSweep: %v", err)
	}
	return sw
}

func seedTestPoint(t *testing.T, st *Store, sweepID, id string, needed, budget int) *TestPoint {
	t.Helper()
	tp := &TestPoint{
		ID: id, SweepID: sweepID, Tier: "small", Workload: "tpcc", Scenario: "x.yaml",
		BoundType: "io", Set: map[string]string{"project_size": "small"},
		SuccessesNeeded: needed, FailureBudget: budget,
	}
	if err := st.CreateTestPoints([]*TestPoint{tp}); err != nil {
		t.Fatalf("CreateTestPoints: %v", err)
	}
	return tp
}

func TestSweep_CreateAndGet(t *testing.T) {
	st := openTestStore(t)
	seedSweep(t, st, "sweep-1")

	got, err := st.GetSweep("sweep-1")
	if err != nil {
		t.Fatalf("GetSweep: %v", err)
	}
	if got.Provider != "AWS" || got.Product != "RDS" || got.Plan != "" || got.Workload != "tpcc" || got.Status != SweepRunning {
		t.Errorf("got = %+v", got)
	}
	if got.HasError() {
		t.Error("fresh sweep should not have an error")
	}
}

func TestSweep_ProductAndPlanRoundTrip(t *testing.T) {
	st := openTestStore(t)
	sw := &Sweep{ID: "sweep-gcp", Provider: "GCP", Product: "Cloud SQL for Postgres", Plan: "Enterprise Plus", Workload: "tpcc", ParamsJSON: "{}", CreatedAt: time.Now().UTC()}
	if err := st.CreateSweep(sw); err != nil {
		t.Fatalf("CreateSweep: %v", err)
	}
	got, err := st.GetSweep("sweep-gcp")
	if err != nil {
		t.Fatalf("GetSweep: %v", err)
	}
	if got.Provider != "GCP" || got.Product != "Cloud SQL for Postgres" || got.Plan != "Enterprise Plus" {
		t.Errorf("got = %+v", got)
	}
}

func TestSweep_GetNotFound(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.GetSweep("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestSweep_RecordAndClearError(t *testing.T) {
	st := openTestStore(t)
	seedSweep(t, st, "sweep-1")

	if err := st.RecordError("sweep-1", "launch", "tp-1", "AWS creds expired"); err != nil {
		t.Fatalf("RecordError: %v", err)
	}
	sw, err := st.GetSweep("sweep-1")
	if err != nil {
		t.Fatal(err)
	}
	if sw.Status != SweepStoppedError || !sw.HasError() {
		t.Errorf("sw = %+v, want stopped_error with an error", sw)
	}
	if sw.ErrorAction != "launch" || sw.ErrorTarget != "tp-1" {
		t.Errorf("ErrorAction/ErrorTarget = %q/%q", sw.ErrorAction, sw.ErrorTarget)
	}
	if sw.ErrorAt == nil {
		t.Error("ErrorAt should be set")
	}

	if err := st.ClearError("sweep-1"); err != nil {
		t.Fatalf("ClearError: %v", err)
	}
	sw, err = st.GetSweep("sweep-1")
	if err != nil {
		t.Fatal(err)
	}
	if sw.Status != SweepRunning || sw.HasError() {
		t.Errorf("sw after clear = %+v", sw)
	}
}

func TestSweep_MarkCompleted(t *testing.T) {
	st := openTestStore(t)
	seedSweep(t, st, "sweep-1")
	if err := st.MarkCompleted("sweep-1"); err != nil {
		t.Fatal(err)
	}
	sw, err := st.GetSweep("sweep-1")
	if err != nil {
		t.Fatal(err)
	}
	if sw.Status != SweepCompleted {
		t.Errorf("Status = %q", sw.Status)
	}

	incomplete, err := st.ListIncompleteSweeps()
	if err != nil {
		t.Fatal(err)
	}
	if len(incomplete) != 0 {
		t.Errorf("ListIncompleteSweeps = %v, want empty", incomplete)
	}
}

func TestListIncompleteSweeps_ExcludesCompleted(t *testing.T) {
	st := openTestStore(t)
	seedSweep(t, st, "sweep-running")
	seedSweep(t, st, "sweep-done")
	if err := st.MarkCompleted("sweep-done"); err != nil {
		t.Fatal(err)
	}

	incomplete, err := st.ListIncompleteSweeps()
	if err != nil {
		t.Fatal(err)
	}
	if len(incomplete) != 1 || incomplete[0].ID != "sweep-running" {
		t.Errorf("incomplete = %v", incomplete)
	}
}

func TestResetSweep_ClearsTestPointsAndRunsAndSweepRow(t *testing.T) {
	st := openTestStore(t)
	seedSweep(t, st, "sweep-1")
	tp := seedTestPoint(t, st, "sweep-1", "sweep-1-small-io", 1, 1)
	run := &Run{RunID: "run-1", TestPointID: tp.ID, IterationAttempt: 1}
	if err := st.CreateRun(run); err != nil {
		t.Fatal(err)
	}
	if err := st.FinalizeRun("run-1", "success"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkCompleted("sweep-1"); err != nil {
		t.Fatal(err)
	}

	if err := st.ResetSweep("sweep-1"); err != nil {
		t.Fatalf("ResetSweep: %v", err)
	}

	if _, err := st.GetSweep("sweep-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSweep after reset = %v, want ErrNotFound", err)
	}
	if tps, err := st.ListTestPoints("sweep-1"); err != nil || len(tps) != 0 {
		t.Errorf("ListTestPoints after reset = %v, %v", tps, err)
	}
	if _, err := st.GetRun("run-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetRun after reset = %v, want ErrNotFound", err)
	}
}

func TestResetSweep_RejectsIncompleteSweep(t *testing.T) {
	st := openTestStore(t)
	seedSweep(t, st, "sweep-1")

	if err := st.ResetSweep("sweep-1"); err == nil {
		t.Error("ResetSweep on a running sweep should have errored")
	}
	// Nothing should have been touched.
	if _, err := st.GetSweep("sweep-1"); err != nil {
		t.Errorf("GetSweep after rejected reset: %v", err)
	}
}

func TestResetSweep_NotFound(t *testing.T) {
	st := openTestStore(t)
	if err := st.ResetSweep("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestDeleteSweep_RemovesEverythingWhateverTheStatus(t *testing.T) {
	statuses := []struct {
		name string
		set  func(t *testing.T, st *Store)
	}{
		{"running", func(t *testing.T, st *Store) {}},
		{"stopped", func(t *testing.T, st *Store) {
			if err := st.RecordError("sweep-1", "teardown", "run-1", "environment still up"); err != nil {
				t.Fatal(err)
			}
		}},
		{"completed", func(t *testing.T, st *Store) {
			if err := st.MarkCompleted("sweep-1"); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range statuses {
		t.Run(tc.name, func(t *testing.T) {
			st := openTestStore(t)
			seedSweep(t, st, "sweep-1")
			tp := seedTestPoint(t, st, "sweep-1", "sweep-1-small-io", 1, 1)
			if err := st.CreateRun(&Run{RunID: "run-1", TestPointID: tp.ID, IterationAttempt: 1}); err != nil {
				t.Fatal(err)
			}
			tc.set(t, st)

			if err := st.DeleteSweep("sweep-1"); err != nil {
				t.Fatalf("DeleteSweep: %v", err)
			}

			if _, err := st.GetSweep("sweep-1"); !errors.Is(err, ErrNotFound) {
				t.Errorf("GetSweep after delete = %v, want ErrNotFound", err)
			}
			if tps, err := st.ListTestPoints("sweep-1"); err != nil || len(tps) != 0 {
				t.Errorf("ListTestPoints after delete = %v, %v", tps, err)
			}
			if _, err := st.GetRun("run-1"); !errors.Is(err, ErrNotFound) {
				t.Errorf("GetRun after delete = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestDeleteSweep_NotFound(t *testing.T) {
	st := openTestStore(t)
	if err := st.DeleteSweep("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// finalizeN creates and finalizes n runs against tp with the given outcome,
// each bumping successes_count or failures_count by one.
func finalizeN(t *testing.T, st *Store, tp *TestPoint, outcome string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		runID := tp.ID + "-" + outcome + "-" + strconv.Itoa(i)
		if err := st.CreateRun(&Run{RunID: runID, TestPointID: tp.ID, IterationAttempt: i + 1}); err != nil {
			t.Fatal(err)
		}
		if err := st.FinalizeRun(runID, outcome); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExtendExhaustedTestPoints_ResetsOnlyExhaustedOnes(t *testing.T) {
	st := openTestStore(t)
	seedSweep(t, st, "sweep-1")
	exhausted := seedTestPoint(t, st, "sweep-1", "sweep-1-exhausted", 5, 2)
	finalizeN(t, st, exhausted, "success", 2)
	finalizeN(t, st, exhausted, "failure", 2) // 2/5 successes, 2/2 failures -> exhausted

	inProgress := seedTestPoint(t, st, "sweep-1", "sweep-1-in-progress", 3, 3)
	finalizeN(t, st, inProgress, "failure", 1) // 0/3 successes, 1/3 failures -> not exhausted

	satisfied := seedTestPoint(t, st, "sweep-1", "sweep-1-satisfied", 1, 1)
	finalizeN(t, st, satisfied, "success", 1) // 1/1 -> satisfied

	if err := st.RecordError("sweep-1", "budget_exhausted", exhausted.ID, "test point exhausted"); err != nil {
		t.Fatal(err)
	}

	if err := st.ExtendExhaustedTestPoints("sweep-1"); err != nil {
		t.Fatalf("ExtendExhaustedTestPoints: %v", err)
	}

	sw, err := st.GetSweep("sweep-1")
	if err != nil {
		t.Fatal(err)
	}
	if sw.Status != SweepRunning || sw.HasError() {
		t.Errorf("sweep after extend = %+v", sw)
	}

	got, err := st.GetTestPoint(exhausted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SuccessesCount != 2 || got.FailuresCount != 0 {
		t.Errorf("exhausted test point after extend = %+v, want successes=2 failures=0", got)
	}

	gotInProgress, err := st.GetTestPoint(inProgress.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotInProgress.FailuresCount != 1 {
		t.Errorf("in-progress test point should be untouched, got %+v", gotInProgress)
	}

	gotSatisfied, err := st.GetTestPoint(satisfied.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotSatisfied.SuccessesCount != 1 {
		t.Errorf("satisfied test point should be untouched, got %+v", gotSatisfied)
	}
}

func TestExtendExhaustedTestPoints_RejectsNonStoppedSweep(t *testing.T) {
	st := openTestStore(t)
	seedSweep(t, st, "sweep-1")
	if err := st.ExtendExhaustedTestPoints("sweep-1"); err == nil {
		t.Error("ExtendExhaustedTestPoints on a running sweep should have errored")
	}
}

func TestExtendExhaustedTestPoints_NotFound(t *testing.T) {
	st := openTestStore(t)
	if err := st.ExtendExhaustedTestPoints("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestRestartSweep_ClearsAllProgressButKeepsTestPointRows(t *testing.T) {
	st := openTestStore(t)
	seedSweep(t, st, "sweep-1")
	exhausted := seedTestPoint(t, st, "sweep-1", "sweep-1-exhausted", 5, 2)
	finalizeN(t, st, exhausted, "success", 2)
	finalizeN(t, st, exhausted, "failure", 2)

	satisfied := seedTestPoint(t, st, "sweep-1", "sweep-1-satisfied", 1, 1)
	finalizeN(t, st, satisfied, "success", 1)

	if err := st.RecordError("sweep-1", "budget_exhausted", exhausted.ID, "test point exhausted"); err != nil {
		t.Fatal(err)
	}
	if err := st.SkipTestPoint("sweep-1", exhausted.ID); err != nil {
		t.Fatalf("SkipTestPoint: %v", err)
	}
	// SkipTestPoint un-stops the sweep as a side effect; re-stop it so
	// RestartSweep's own precondition (status stopped_error) is met, exactly
	// as a real recovery flow would after a test point re-exhausts.
	if err := st.RecordError("sweep-1", "budget_exhausted", exhausted.ID, "test point exhausted"); err != nil {
		t.Fatal(err)
	}

	if err := st.RestartSweep("sweep-1"); err != nil {
		t.Fatalf("RestartSweep: %v", err)
	}

	sw, err := st.GetSweep("sweep-1")
	if err != nil {
		t.Fatal(err)
	}
	if sw.Status != SweepRunning || sw.HasError() {
		t.Errorf("sweep after restart = %+v", sw)
	}

	tps, err := st.ListTestPoints("sweep-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(tps) != 2 {
		t.Fatalf("len(tps) after restart = %d, want 2 (rows kept, only counters reset)", len(tps))
	}
	for _, tp := range tps {
		if tp.SuccessesCount != 0 || tp.FailuresCount != 0 {
			t.Errorf("tp %s after restart = %+v, want zeroed counters", tp.ID, tp)
		}
		if tp.Skipped {
			t.Errorf("tp %s after restart = %+v, want Skipped cleared (start fresh redoes everything)", tp.ID, tp)
		}
	}

	if _, err := st.GetRun(exhausted.ID + "-success-0"); !errors.Is(err, ErrNotFound) {
		t.Errorf("old run should be deleted, got err = %v", err)
	}
	if _, err := st.GetRun(satisfied.ID + "-success-0"); !errors.Is(err, ErrNotFound) {
		t.Errorf("old run should be deleted, got err = %v", err)
	}
}

func TestRestartSweep_RejectsNonStoppedSweep(t *testing.T) {
	st := openTestStore(t)
	seedSweep(t, st, "sweep-1")
	if err := st.RestartSweep("sweep-1"); err == nil {
		t.Error("RestartSweep on a running sweep should have errored")
	}
}

func TestRestartSweep_NotFound(t *testing.T) {
	st := openTestStore(t)
	if err := st.RestartSweep("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestSkipTestPoint_SetsFlagOnlyOnTargetAndUnstopsSweep(t *testing.T) {
	st := openTestStore(t)
	seedSweep(t, st, "sweep-1")
	exhausted := seedTestPoint(t, st, "sweep-1", "sweep-1-exhausted", 5, 2)
	finalizeN(t, st, exhausted, "success", 2)
	finalizeN(t, st, exhausted, "failure", 2) // 2/5 successes, 2/2 failures -> exhausted

	other := seedTestPoint(t, st, "sweep-1", "sweep-1-other", 3, 3)
	finalizeN(t, st, other, "failure", 1)

	if err := st.RecordError("sweep-1", "budget_exhausted", exhausted.ID, "test point exhausted"); err != nil {
		t.Fatal(err)
	}

	if err := st.SkipTestPoint("sweep-1", exhausted.ID); err != nil {
		t.Fatalf("SkipTestPoint: %v", err)
	}

	sw, err := st.GetSweep("sweep-1")
	if err != nil {
		t.Fatal(err)
	}
	if sw.Status != SweepRunning || sw.HasError() {
		t.Errorf("sweep after skip = %+v", sw)
	}

	got, err := st.GetTestPoint(exhausted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Skipped {
		t.Errorf("skipped test point = %+v, want Skipped=true", got)
	}
	if got.SuccessesCount != 2 || got.FailuresCount != 2 {
		t.Errorf("skip should leave existing counters untouched, got %+v", got)
	}

	gotOther, err := st.GetTestPoint(other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotOther.Skipped {
		t.Errorf("other test point should be untouched, got %+v", gotOther)
	}
}

func TestSkipTestPoint_RejectsNonStoppedSweep(t *testing.T) {
	st := openTestStore(t)
	seedSweep(t, st, "sweep-1")
	tp := seedTestPoint(t, st, "sweep-1", "sweep-1-tp", 1, 1)
	if err := st.SkipTestPoint("sweep-1", tp.ID); err == nil {
		t.Error("SkipTestPoint on a running sweep should have errored")
	}
}

func TestSkipTestPoint_NotFound(t *testing.T) {
	st := openTestStore(t)
	if err := st.SkipTestPoint("nope", "nope-tp"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestFinalizeRun_SkippedTestPointCountersFrozen(t *testing.T) {
	st := openTestStore(t)
	seedSweep(t, st, "sweep-1")
	tp := seedTestPoint(t, st, "sweep-1", "sweep-1-flaky", 3, 3)
	finalizeN(t, st, tp, "failure", 3) // exhausts the budget: 0/3 successes, 3/3 failures

	if err := st.RecordError("sweep-1", "budget_exhausted", tp.ID, "test point exhausted"); err != nil {
		t.Fatal(err)
	}
	if err := st.SkipTestPoint("sweep-1", tp.ID); err != nil {
		t.Fatalf("SkipTestPoint: %v", err)
	}

	// A straggler run created before the skip decision (e.g. a second
	// concurrent attempt) can still finalize afterward -- its environment
	// still needs tearing down, which is what calls FinalizeRun, but the
	// now-skipped test point's counters must not move.
	straggler := &Run{RunID: tp.ID + "-straggler", TestPointID: tp.ID, IterationAttempt: 4}
	if err := st.CreateRun(straggler); err != nil {
		t.Fatal(err)
	}
	if err := st.FinalizeRun(straggler.RunID, "failure"); err != nil {
		t.Fatalf("FinalizeRun: %v", err)
	}

	got, err := st.GetTestPoint(tp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FailuresCount != 3 {
		t.Errorf("FailuresCount = %d, want 3 (frozen at skip time, not bumped by a post-skip straggler)", got.FailuresCount)
	}
	if !got.Skipped {
		t.Errorf("test point should still be skipped, got %+v", got)
	}

	run, err := st.GetRun(straggler.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunFailed {
		t.Errorf("straggler run status = %q, want failed (still reconciled/torn down despite the frozen counter)", run.Status)
	}
}

func TestUpdateParamsJSON_RoundTrips(t *testing.T) {
	st := openTestStore(t)
	seedSweep(t, st, "sweep-1")

	if err := st.UpdateParamsJSON("sweep-1", `{"max_concurrency":8}`); err != nil {
		t.Fatalf("UpdateParamsJSON: %v", err)
	}

	sw, err := st.GetSweep("sweep-1")
	if err != nil {
		t.Fatal(err)
	}
	if sw.ParamsJSON != `{"max_concurrency":8}` {
		t.Errorf("ParamsJSON = %q, want the updated blob", sw.ParamsJSON)
	}
}

func TestUpdateParamsJSON_NotFound(t *testing.T) {
	st := openTestStore(t)
	if err := st.UpdateParamsJSON("nope", "{}"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestTestPoint_CreateAndList(t *testing.T) {
	st := openTestStore(t)
	seedSweep(t, st, "sweep-1")
	seedTestPoint(t, st, "sweep-1", "sweep-1-small-io", 5, 5)

	tps, err := st.ListTestPoints("sweep-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(tps) != 1 {
		t.Fatalf("len(tps) = %d, want 1", len(tps))
	}
	tp := tps[0]
	if tp.Tier != "small" || tp.Set["project_size"] != "small" {
		t.Errorf("tp = %+v", tp)
	}
	if tp.Satisfied() {
		t.Error("fresh test point should not be satisfied")
	}
	if tp.NextAttempt() != 1 {
		t.Errorf("NextAttempt() = %d, want 1", tp.NextAttempt())
	}
}

func TestRun_CreateGetAndStatus(t *testing.T) {
	st := openTestStore(t)
	seedSweep(t, st, "sweep-1")
	tp := seedTestPoint(t, st, "sweep-1", "sweep-1-small-io", 5, 5)

	run := &Run{RunID: "run-1", TestPointID: tp.ID, IterationAttempt: 1}
	if err := st.CreateRun(run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if run.Status != RunLaunching {
		t.Errorf("Status after create = %q", run.Status)
	}

	got, err := st.GetRun("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != RunLaunching {
		t.Errorf("got.Status = %q", got.Status)
	}

	if err := st.SetRunStatus("run-1", RunWaitingRemote); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetRun("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != RunWaitingRemote {
		t.Errorf("Status after transition = %q", got.Status)
	}
}

func TestSetRunStatus_UnknownRunErrors(t *testing.T) {
	st := openTestStore(t)
	if err := st.SetRunStatus("nope", RunWaitingRemote); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestListNonTerminalRuns(t *testing.T) {
	st := openTestStore(t)
	seedSweep(t, st, "sweep-1")
	tp := seedTestPoint(t, st, "sweep-1", "sweep-1-small-io", 5, 5)

	active := &Run{RunID: "run-active", TestPointID: tp.ID, IterationAttempt: 1}
	done := &Run{RunID: "run-done", TestPointID: tp.ID, IterationAttempt: 2}
	if err := st.CreateRun(active); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRun(done); err != nil {
		t.Fatal(err)
	}
	if err := st.FinalizeRun("run-done", "success"); err != nil {
		t.Fatal(err)
	}

	runs, err := st.ListNonTerminalRuns("sweep-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].RunID != "run-active" {
		t.Errorf("ListNonTerminalRuns = %v", runs)
	}
}

func TestListRunsForTestPoint(t *testing.T) {
	st := openTestStore(t)
	seedSweep(t, st, "sweep-1")
	tp := seedTestPoint(t, st, "sweep-1", "sweep-1-small-io", 5, 5)
	otherTP := seedTestPoint(t, st, "sweep-1", "sweep-1-small-compute", 5, 5)

	failed := &Run{RunID: "run-failed", TestPointID: tp.ID, IterationAttempt: 1}
	active := &Run{RunID: "run-active", TestPointID: tp.ID, IterationAttempt: 2}
	done := &Run{RunID: "run-done", TestPointID: tp.ID, IterationAttempt: 3}
	other := &Run{RunID: "run-other-tp", TestPointID: otherTP.ID, IterationAttempt: 1}
	for _, r := range []*Run{failed, active, done, other} {
		if err := st.CreateRun(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.FinalizeRun("run-failed", "failure"); err != nil {
		t.Fatal(err)
	}
	if err := st.FinalizeRun("run-done", "success"); err != nil {
		t.Fatal(err)
	}

	runs, err := st.ListRunsForTestPoint(tp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Fatalf("ListRunsForTestPoint = %d runs, want 3: %+v", len(runs), runs)
	}
	wantIDs := []string{"run-failed", "run-active", "run-done"}
	for i, want := range wantIDs {
		if runs[i].RunID != want || runs[i].IterationAttempt != i+1 {
			t.Errorf("runs[%d] = %+v, want RunID=%s IterationAttempt=%d", i, runs[i], want, i+1)
		}
	}
	if runs[0].Status != RunFailed || runs[0].Outcome != "failure" {
		t.Errorf("runs[0] (failed) = %+v", runs[0])
	}
	if runs[1].Status != RunLaunching {
		t.Errorf("runs[1] (active) = %+v", runs[1])
	}
	if runs[2].Status != RunDone || runs[2].Outcome != "success" {
		t.Errorf("runs[2] (done) = %+v", runs[2])
	}

	empty, err := st.ListRunsForTestPoint("no-such-test-point")
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Errorf("ListRunsForTestPoint(unknown) = %v, want empty", empty)
	}
}

func TestFinalizeRun_SuccessIncrementsSuccessesCount(t *testing.T) {
	st := openTestStore(t)
	seedSweep(t, st, "sweep-1")
	tp := seedTestPoint(t, st, "sweep-1", "sweep-1-small-io", 5, 5)
	run := &Run{RunID: "run-1", TestPointID: tp.ID, IterationAttempt: 1}
	if err := st.CreateRun(run); err != nil {
		t.Fatal(err)
	}

	if err := st.FinalizeRun("run-1", "success"); err != nil {
		t.Fatalf("FinalizeRun: %v", err)
	}

	got, err := st.GetRun("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != RunDone || got.Outcome != "success" {
		t.Errorf("run after finalize = %+v", got)
	}

	tpAfter, err := st.GetTestPoint(tp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tpAfter.SuccessesCount != 1 || tpAfter.FailuresCount != 0 {
		t.Errorf("tp after finalize = %+v", tpAfter)
	}
}

func TestFinalizeRun_FailureIncrementsFailuresCount(t *testing.T) {
	st := openTestStore(t)
	seedSweep(t, st, "sweep-1")
	tp := seedTestPoint(t, st, "sweep-1", "sweep-1-small-io", 5, 5)
	run := &Run{RunID: "run-1", TestPointID: tp.ID, IterationAttempt: 1}
	if err := st.CreateRun(run); err != nil {
		t.Fatal(err)
	}

	if err := st.FinalizeRun("run-1", "failure"); err != nil {
		t.Fatalf("FinalizeRun: %v", err)
	}

	tpAfter, err := st.GetTestPoint(tp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tpAfter.FailuresCount != 1 || tpAfter.SuccessesCount != 0 {
		t.Errorf("tp after finalize = %+v", tpAfter)
	}
	got, _ := st.GetRun("run-1")
	if got.Status != RunFailed {
		t.Errorf("Status = %q, want failed", got.Status)
	}
}

func TestFinalizeRun_InvalidOutcomeRejected(t *testing.T) {
	st := openTestStore(t)
	seedSweep(t, st, "sweep-1")
	tp := seedTestPoint(t, st, "sweep-1", "sweep-1-small-io", 5, 5)
	run := &Run{RunID: "run-1", TestPointID: tp.ID, IterationAttempt: 1}
	if err := st.CreateRun(run); err != nil {
		t.Fatal(err)
	}
	if err := st.FinalizeRun("run-1", "maybe"); err == nil {
		t.Fatal("expected error for invalid outcome")
	}
}

func TestFinalizeRun_UnknownRunErrors(t *testing.T) {
	st := openTestStore(t)
	if err := st.FinalizeRun("nope", "success"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestIncrementFetchAttempts(t *testing.T) {
	st := openTestStore(t)
	seedSweep(t, st, "sweep-1")
	tp := seedTestPoint(t, st, "sweep-1", "sweep-1-small-io", 5, 5)
	run := &Run{RunID: "run-1", TestPointID: tp.ID, IterationAttempt: 1}
	if err := st.CreateRun(run); err != nil {
		t.Fatal(err)
	}

	for want := 1; want <= 3; want++ {
		got, err := st.IncrementFetchAttempts("run-1")
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("attempt %d: got %d", want, got)
		}
	}
}

func TestSetRunArtifactDirAndOutcome(t *testing.T) {
	st := openTestStore(t)
	seedSweep(t, st, "sweep-1")
	tp := seedTestPoint(t, st, "sweep-1", "sweep-1-small-io", 5, 5)
	run := &Run{RunID: "run-1", TestPointID: tp.ID, IterationAttempt: 1}
	if err := st.CreateRun(run); err != nil {
		t.Fatal(err)
	}

	if err := st.SetRunArtifactDir("run-1", "/tmp/artifacts/run-1"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunOutcome("run-1", "success"); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetRun("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.LocalArtifactDir != "/tmp/artifacts/run-1" || got.Outcome != "success" {
		t.Errorf("got = %+v", got)
	}
}

func TestTestPoint_BudgetExhausted(t *testing.T) {
	st := openTestStore(t)
	seedSweep(t, st, "sweep-1")
	tp := seedTestPoint(t, st, "sweep-1", "sweep-1-small-io", 5, 2)

	for i := 0; i < 2; i++ {
		run := &Run{RunID: "run-fail-" + string(rune('a'+i)), TestPointID: tp.ID, IterationAttempt: i + 1}
		if err := st.CreateRun(run); err != nil {
			t.Fatal(err)
		}
		if err := st.FinalizeRun(run.RunID, "failure"); err != nil {
			t.Fatal(err)
		}
	}

	tpAfter, err := st.GetTestPoint(tp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !tpAfter.BudgetExhausted() {
		t.Error("expected budget to be exhausted after 2 failures with a budget of 2")
	}
	if tpAfter.Satisfied() {
		t.Error("test point should not be satisfied -- it never succeeded")
	}
}

func TestOpen_PersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dbarenactl.db")

	st1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	seedSweep(t, st1, "sweep-1")
	st1.Close()

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	sw, err := st2.GetSweep("sweep-1")
	if err != nil {
		t.Fatalf("GetSweep after reopen: %v", err)
	}
	if sw.Provider != "AWS" {
		t.Errorf("sw = %+v", sw)
	}
}

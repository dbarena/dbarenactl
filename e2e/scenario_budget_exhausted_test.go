//go:build e2e

package e2e

import (
	"testing"
	"time"

	"github.com/dbarena/dbarenactl/internal/scheduler"
	"github.com/dbarena/dbarenactl/internal/sweepstate"
)

// TestBudgetExhausted_StopsWhileAnotherRunIsInFlight_ThenResumeContinuePicksItUp
// reproduces the behavior found by hand this session: one test point fails
// every attempt and exhausts its failure budget while another test point's
// benchmark is still executing. The whole sweep stops immediately -- the
// still-executing run is left non-terminal, not yet fetched or torn down.
//
// Its worker is deliberately left running (not killed): once it finishes
// for real, resuming in "continue" mode should pick it up on the very next
// pass and tear it down cleanly, even though the persistently-failing test
// point re-exhausts its (fresh) budget again immediately after.
func TestBudgetExhausted_StopsWhileAnotherRunIsInFlight_ThenResumeContinuePicksItUp(t *testing.T) {
	slow := testPointSpec{Tier: "small", BoundType: "io"}
	failing := testPointSpec{Tier: "small", BoundType: "compute"}
	fast := testPointSpec{Tier: "medium", BoundType: "io"}
	points := []testPointSpec{slow, failing, fast}
	manifestPath := writeManifest(t, "budget-exhausted", points)

	env := newEnv(t, fakeConfig{
		// Bootstrap/teardown kept short so the persistently-failing test
		// point can burn through several attempts quickly, while the slow
		// test point's workload duration is set far longer than that whole
		// cycle -- so the budget reliably exhausts while it's still running,
		// not by a race between the two.
		BootstrapDuration: "50ms",
		TeardownDuration:  "50ms",
		TestPoints: map[string]behavior{
			"small/io":      {Outcome: "success", WorkloadDuration: "5s"},
			"small/compute": {Outcome: "fail", WorkloadDuration: "50ms"},
			"medium/io":     {Outcome: "success", WorkloadDuration: "50ms"},
		},
	})

	const maxConcurrency = 3
	const iterations = 1
	const maxWorkloadFailures = 3

	res := runDbarenactl(t, env.vars(), "y\n",
		"run",
		"--candidate", manifestPath,
		"--max-concurrency", itoa(maxConcurrency),
		"--iterations", itoa(iterations),
		"--max-workload-failures", itoa(maxWorkloadFailures),
	)
	if res.ExitCode == 0 {
		t.Fatalf("dbarenactl run: exit code 0, want nonzero (budget should exhaust)\nstdout:\n%s", res.Stdout)
	}

	sweepID := sweepIDFor(t, manifestPath, "budget-exhausted", iterations, maxWorkloadFailures)
	store := openStore(t, env.dbHome)

	sweep, err := store.GetSweep(sweepID)
	if err != nil {
		t.Fatalf("GetSweep: %v", err)
	}
	if sweep.ErrorAction != scheduler.ActionBudgetExhausted {
		t.Fatalf("ErrorAction = %q, want %q (detail: %s)", sweep.ErrorAction, scheduler.ActionBudgetExhausted, sweep.ErrorDetail)
	}
	if sweep.ErrorTarget != testPointID(sweepID, failing) {
		t.Errorf("ErrorTarget = %q, want the failing test point", sweep.ErrorTarget)
	}

	active, err := store.ListNonTerminalRuns(sweepID)
	if err != nil {
		t.Fatalf("ListNonTerminalRuns: %v", err)
	}
	var slowRun *sweepstate.Run
	slowTPID := testPointID(sweepID, slow)
	for _, r := range active {
		if r.TestPointID == slowTPID {
			slowRun = r
		}
	}
	if slowRun == nil {
		t.Fatalf("the slow test point's run should still be non-terminal when the sweep stopped, found: %+v", active)
	}
	if slowRun.Status.Terminal() {
		t.Fatalf("slow run status = %q, want non-terminal", slowRun.Status)
	}

	// The slow run's worker was never killed -- let it actually finish on
	// its own before resuming, exactly as done by hand this session.
	waitForWorkloadCompletion(t, env.stateDir, slowRun.RunID, 10*time.Second)

	resumeRes := runDbarenactl(t, env.vars(), "c\nr\n", "resume", sweepID)
	if resumeRes.ExitCode == 0 {
		t.Fatalf("dbarenactl resume: exit code 0, want nonzero (the failing test point re-exhausts immediately)\nstdout:\n%s", resumeRes.Stdout)
	}

	slowTP, err := store.GetTestPoint(slowTPID)
	if err != nil {
		t.Fatalf("GetTestPoint(slow): %v", err)
	}
	if !slowTP.Satisfied() {
		t.Errorf("slow test point not satisfied after resume: %d/%d successes", slowTP.SuccessesCount, slowTP.SuccessesNeeded)
	}
	slowRunAfter, err := store.GetRun(slowRun.RunID)
	if err != nil {
		t.Fatalf("GetRun(slow): %v", err)
	}
	if slowRunAfter.Status != sweepstate.RunDone {
		t.Errorf("slow run status after resume = %q, want %q (should have been fetched and torn down)", slowRunAfter.Status, sweepstate.RunDone)
	}

	sweepAfter, err := store.GetSweep(sweepID)
	if err != nil {
		t.Fatalf("GetSweep after resume: %v", err)
	}
	if sweepAfter.ErrorAction != scheduler.ActionBudgetExhausted {
		t.Errorf("ErrorAction after resume = %q, want %q again (the failing test point never succeeds)", sweepAfter.ErrorAction, scheduler.ActionBudgetExhausted)
	}
}

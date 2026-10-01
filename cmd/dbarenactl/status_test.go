package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/dbarena/dbarenactl/internal/scheduler"
	"github.com/dbarena/dbarenactl/internal/sweepstate"
)

// TestComputeProgress_ClampsToLongestInFlightRun guards against the overall
// sweep ETA under-counting the tail of a sweep: dividing the remaining work
// across all concurrency slots assumes that work can still be freely spread
// out, but runs already in flight are pinned to their own remaining time
// regardless of how much concurrency the sweep has overall. Reproduces the
// reported case (two concurrent attempts on the same test point, overall
// ETA computed as less than either run's own remaining time).
func TestComputeProgress_ClampsToLongestInFlightRun(t *testing.T) {
	st, err := sweepstate.Open(filepath.Join(t.TempDir(), "dbarenactl.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	sw := &sweepstate.Sweep{ID: "sweep-1", Provider: "AWS", ParamsJSON: `{"max_concurrency": 4}`, CreatedAt: time.Now().UTC()}
	if err := st.CreateSweep(sw); err != nil {
		t.Fatal(err)
	}

	tp := &sweepstate.TestPoint{
		ID: "sweep-1-xlarge-cache-exceeding", SweepID: sw.ID, Tier: "xlarge", Workload: "tpcc",
		Scenario: "x.yaml", BoundType: "cache-exceeding", Set: map[string]string{"project_size": "xlarge"},
		SuccessesNeeded: 3, FailureBudget: 3,
	}
	if err := st.CreateTestPoints([]*sweepstate.TestPoint{tp}); err != nil {
		t.Fatal(err)
	}

	// One completed run establishes a non-zero average duration for this
	// test point.
	baseline := &sweepstate.Run{RunID: "run-1", TestPointID: tp.ID, IterationAttempt: 1}
	if err := st.CreateRun(baseline); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	if err := st.FinalizeRun(baseline.RunID, "success"); err != nil {
		t.Fatal(err)
	}

	// Two more attempts running concurrently right now (attemptsLeft == 2),
	// mirroring the reported xlarge/cache-exceeding case.
	for _, id := range []string{"run-2", "run-3"} {
		run := &sweepstate.Run{RunID: id, TestPointID: tp.ID, IterationAttempt: 1}
		if err := st.CreateRun(run); err != nil {
			t.Fatal(err)
		}
	}

	runs, err := st.ListRunsForSweep(sw.ID)
	if err != nil {
		t.Fatal(err)
	}
	avg := scheduler.AvgSuccessDurations(runs)[tp.ID]
	if avg <= 0 {
		t.Fatalf("expected a positive baseline average, got %v", avg)
	}

	p, err := computeProgress(st, sw)
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasETA {
		t.Fatal("expected HasETA to be true")
	}
	if p.Done != 1 || p.Total != 3 {
		t.Fatalf("Done/Total = %d/%d, want 1/3", p.Done, p.Total)
	}

	// The naive remaining/concurrency formula would give roughly avg/2 here
	// (2 attempts' worth of work divided by 4 concurrency slots), which is
	// less than either in-flight run's own remaining time (~avg). The fix
	// clamps the overall ETA to at least the longest in-flight run's
	// remaining time.
	if min := avg * 7 / 10; p.ETA < min {
		t.Fatalf("ETA = %v, want at least %v (avg %v)", p.ETA, min, avg)
	}
}

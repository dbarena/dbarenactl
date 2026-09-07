//go:build e2e

package e2e

import "testing"

// TestKnownGood_MultipleTestPointsAllSucceed drives a full sweep across
// several test points that all succeed, with fewer concurrency slots than
// test points (so queuing is exercised too) and more than one required
// success per test point (so retrying-for-success, not just
// retrying-for-failure, is exercised). The sweep should finish cleanly with
// nothing left running.
func TestKnownGood_MultipleTestPointsAllSucceed(t *testing.T) {
	points := []testPointSpec{
		{Tier: "small", BoundType: "io"},
		{Tier: "small", BoundType: "compute"},
		{Tier: "medium", BoundType: "io"},
		{Tier: "medium", BoundType: "compute"},
	}
	manifestPath := writeManifest(t, "known-good", points)

	env := newEnv(t, fakeConfig{
		Default: behavior{Outcome: "success", WorkloadDuration: "50ms"},
	})

	const maxConcurrency = 2
	const iterations = 2

	res := runDbarenactl(t, env.vars(), "",
		"run",
		"--candidate", manifestPath,
		"--max-concurrency", itoa(maxConcurrency),
		"--iterations", itoa(iterations),
	)
	if res.ExitCode != 0 {
		t.Fatalf("dbarenactl run: exit code %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}

	sweepID := sweepIDFor(t, manifestPath, "known-good", iterations, 0)
	store := openStore(t, env.dbHome)

	sweep, err := store.GetSweep(sweepID)
	if err != nil {
		t.Fatalf("GetSweep: %v", err)
	}
	if sweep.Status != "completed" {
		t.Errorf("sweep status = %q, want completed (error: %s / %s)", sweep.Status, sweep.ErrorAction, sweep.ErrorDetail)
	}

	testPoints, err := store.ListTestPoints(sweepID)
	if err != nil {
		t.Fatalf("ListTestPoints: %v", err)
	}
	if len(testPoints) != len(points) {
		t.Fatalf("len(testPoints) = %d, want %d", len(testPoints), len(points))
	}
	for _, tp := range testPoints {
		if tp.SuccessesCount != iterations {
			t.Errorf("test point %s: SuccessesCount = %d, want %d", tp.ID, tp.SuccessesCount, iterations)
		}
		if tp.FailuresCount != 0 {
			t.Errorf("test point %s: FailuresCount = %d, want 0", tp.ID, tp.FailuresCount)
		}
	}

	active, err := store.ListNonTerminalRuns(sweepID)
	if err != nil {
		t.Fatalf("ListNonTerminalRuns: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("ListNonTerminalRuns = %d, want 0 (nothing should still be running)", len(active))
	}
}

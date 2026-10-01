//go:build e2e

package e2e

import (
	"testing"

	"github.com/dbarena/dbarenactl/internal/scheduler"
	"github.com/dbarena/dbarenactl/internal/sweepstate"
)

// TestLaunchFailure_StopsImmediately covers the case where an environment
// never starts at all: benchctl fails during the blocking bootstrap step.
// Unlike a workload failure, this is never retried against a budget -- one
// failure is enough to stop the whole sweep.
func TestLaunchFailure_StopsImmediately(t *testing.T) {
	broken := testPointSpec{Tier: "small", BoundType: "io"}
	manifestPath := writeManifest(t, "launch-failure", []testPointSpec{broken})

	env := newEnv(t, fakeConfig{
		BootstrapDuration: "50ms",
		TestPoints: map[string]behavior{
			"small/io": {LaunchFails: true},
		},
	})

	const maxConcurrency = 1
	const iterations = 1

	res := runDbarenactl(t, env.vars(), "y\n",
		"run",
		"--candidate", manifestPath,
		"--max-concurrency", itoa(maxConcurrency),
		"--iterations", itoa(iterations),
	)
	if res.ExitCode == 0 {
		t.Fatalf("dbarenactl run: exit code 0, want nonzero (launch should fail)\nstdout:\n%s", res.Stdout)
	}

	sweepID := sweepIDFor(t, manifestPath, "launch-failure", iterations, 0)
	store := openStore(t, env.dbHome)

	sweep, err := store.GetSweep(sweepID)
	if err != nil {
		t.Fatalf("GetSweep: %v", err)
	}
	if sweep.ErrorAction != scheduler.ActionLaunch {
		t.Fatalf("ErrorAction = %q, want %q (detail: %s)", sweep.ErrorAction, scheduler.ActionLaunch, sweep.ErrorDetail)
	}

	// dbarenactl records intent to launch *before* invoking benchctl at all
	// (see internal/scheduler.Scheduler.launch), so the row survives the
	// failed attempt in "launching" -- it never got far enough to know
	// whether benchctl has any record of it, so it can't be discarded
	// without checking first (that happens on the next `resume`, not here).
	active, err := store.ListNonTerminalRuns(sweepID)
	if err != nil {
		t.Fatalf("ListNonTerminalRuns: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("ListNonTerminalRuns = %d, want 1", len(active))
	}
	if active[0].Status != sweepstate.RunLaunching {
		t.Errorf("run status = %q, want %q", active[0].Status, sweepstate.RunLaunching)
	}
}

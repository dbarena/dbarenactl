//go:build e2e

package e2e

import (
	"testing"

	"github.com/dbarena/dbarenactl/internal/scheduler"
	"github.com/dbarena/dbarenactl/internal/sweepstate"
)

// TestFetchFailure_ExhaustsRetriesThenStops covers a benchmark that finishes
// fine, but whose results can never be pulled. dbarenactl retries a bounded
// number of times, then stops the sweep -- with the environment still up,
// since fetch failing means teardown is never reached.
func TestFetchFailure_ExhaustsRetriesThenStops(t *testing.T) {
	broken := testPointSpec{Tier: "small", BoundType: "io"}
	manifestPath := writeManifest(t, "fetch-failure", []testPointSpec{broken})

	env := newEnv(t, fakeConfig{
		BootstrapDuration: "50ms",
		TeardownDuration:  "50ms",
		TestPoints: map[string]behavior{
			"small/io": {Outcome: "success", WorkloadDuration: "50ms", FetchFails: true},
		},
	})

	const maxConcurrency = 1
	const iterations = 1

	res := runDbarenactl(t, env.vars(), "",
		"run",
		"--candidate", manifestPath,
		"--max-concurrency", itoa(maxConcurrency),
		"--iterations", itoa(iterations),
	)
	if res.ExitCode == 0 {
		t.Fatalf("dbarenactl run: exit code 0, want nonzero (fetch should exhaust its retries)\nstdout:\n%s", res.Stdout)
	}

	sweepID := sweepIDFor(t, manifestPath, "fetch-failure", iterations, 0)
	store := openStore(t, env.dbHome)

	sweep, err := store.GetSweep(sweepID)
	if err != nil {
		t.Fatalf("GetSweep: %v", err)
	}
	if sweep.ErrorAction != scheduler.ActionFetch {
		t.Fatalf("ErrorAction = %q, want %q (detail: %s)", sweep.ErrorAction, scheduler.ActionFetch, sweep.ErrorDetail)
	}

	active, err := store.ListNonTerminalRuns(sweepID)
	if err != nil {
		t.Fatalf("ListNonTerminalRuns: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("ListNonTerminalRuns = %d, want 1", len(active))
	}
	// Still waiting on a successful results pull -- teardown must never
	// have been reached for this run.
	if active[0].Status != sweepstate.RunNeedsResultsPull {
		t.Errorf("run status = %q, want %q (teardown should not have been attempted)", active[0].Status, sweepstate.RunNeedsResultsPull)
	}
}

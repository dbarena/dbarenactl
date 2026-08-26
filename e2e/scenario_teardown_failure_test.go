//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dbarena/dbarenactl/internal/scheduler"
	"github.com/dbarena/dbarenactl/internal/sweepstate"
)

// TestTeardownFailure_StopsWithEnvironmentFateUnclear covers a benchmark
// that finishes and whose results are fetched fine, but whose environment
// can't be torn down. The sweep stops with that environment's state left
// unresolved -- this is the concrete case troubleshooting.md calls out as
// "don't assume it's gone, check directly".
func TestTeardownFailure_StopsWithEnvironmentFateUnclear(t *testing.T) {
	broken := testPointSpec{Tier: "small", BoundType: "io"}
	manifestPath := writeManifest(t, "teardown-failure", []testPointSpec{broken})

	env := newEnv(t, fakeConfig{
		BootstrapDuration: "50ms",
		TeardownDuration:  "50ms",
		TestPoints: map[string]behavior{
			"small/io": {Outcome: "success", WorkloadDuration: "50ms", TeardownFails: true},
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
		t.Fatalf("dbarenactl run: exit code 0, want nonzero (teardown should fail)\nstdout:\n%s", res.Stdout)
	}

	sweepID := sweepIDFor(t, manifestPath, "teardown-failure", maxConcurrency, iterations, 0)
	store := openStore(t, env.dbHome)

	sweep, err := store.GetSweep(sweepID)
	if err != nil {
		t.Fatalf("GetSweep: %v", err)
	}
	if sweep.ErrorAction != scheduler.ActionTeardown {
		t.Fatalf("ErrorAction = %q, want %q (detail: %s)", sweep.ErrorAction, scheduler.ActionTeardown, sweep.ErrorDetail)
	}

	active, err := store.ListNonTerminalRuns(sweepID)
	if err != nil {
		t.Fatalf("ListNonTerminalRuns: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("ListNonTerminalRuns = %d, want 1", len(active))
	}
	run := active[0]
	if run.Status != sweepstate.RunNeedsTeardown {
		t.Errorf("run status = %q, want %q", run.Status, sweepstate.RunNeedsTeardown)
	}

	// Confirm directly against the fake's own state (standing in for
	// benchctl) that the environment really is still reported up: the
	// teardown phase was attempted but never completed.
	data, err := os.ReadFile(filepath.Join(env.stateDir, run.RunID+".json"))
	if err != nil {
		t.Fatalf("read fake state for %s: %v", run.RunID, err)
	}
	var st struct {
		Phases       map[string]string `json:"phases"`
		TerminatedAt *string           `json:"terminated_at"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("parse fake state: %v", err)
	}
	if st.Phases["teardown"] == "completed" {
		t.Errorf("fake state shows teardown completed, want it never to have completed")
	}
	if st.TerminatedAt != nil {
		t.Errorf("fake state has terminated_at set, want nil (environment never actually came down)")
	}
}

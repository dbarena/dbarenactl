//go:build e2e

package e2e

import (
	"testing"

	"github.com/dbarena/dbarenactl/internal/scheduler"
)

// TestStaleHeartbeat_FailsRunAndTearsDown covers a driver that stops reporting
// mid-workload. dbarenactl cannot tell a dead driver from a live one that can
// no longer write to benchctl's store, and neither can ever reach a terminal
// state, so it fails the run and tears the environment down rather than
// waiting forever. The run then behaves like any other failed workload: it
// charges the test point's failure budget, and exhausting that budget stops
// the sweep.
//
// The single most important assertion here is that the command terminates at
// all -- before this behavior existed the sweep hung on the stale run
// indefinitely.
func TestStaleHeartbeat_FailsRunAndTearsDown(t *testing.T) {
	// Both branches of the best-effort diagnostics fetch: a driver instance
	// that is still reachable, and one that is gone with its artifacts.
	for _, tc := range []struct {
		name       string
		fetchFails bool
	}{
		{"driver reachable but reporting-blind", false},
		{"driver gone", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stalling := testPointSpec{Tier: "small", BoundType: "io"}
			manifestPath := writeManifest(t, "stale-heartbeat", []testPointSpec{stalling})

			env := newEnv(t, fakeConfig{
				BootstrapDuration: "50ms",
				TeardownDuration:  "50ms",
				TestPoints: map[string]behavior{
					"small/io": {
						Outcome:        "success",
						StaleHeartbeat: true,
						FetchFails:     tc.fetchFails,
					},
				},
			})

			const iterations = 1
			const maxWorkloadFailures = 1

			res := runDbarenactl(t, env.vars(), "y\n",
				"run",
				"--candidate", manifestPath,
				"--max-concurrency", itoa(1),
				"--iterations", itoa(iterations),
				"--max-workload-failures", itoa(maxWorkloadFailures),
			)
			if res.ExitCode == 0 {
				t.Fatalf("dbarenactl run: exit code 0, want nonzero (the failure budget should be exhausted)\nstdout:\n%s", res.Stdout)
			}

			sweepID := sweepIDFor(t, manifestPath, "stale-heartbeat", iterations, maxWorkloadFailures)
			store := openStore(t, env.dbHome)

			sweep, err := store.GetSweep(sweepID)
			if err != nil {
				t.Fatalf("GetSweep: %v", err)
			}
			if sweep.ErrorAction != scheduler.ActionBudgetExhausted {
				t.Fatalf("ErrorAction = %q, want %q (detail: %s)", sweep.ErrorAction, scheduler.ActionBudgetExhausted, sweep.ErrorDetail)
			}

			// Nothing left in flight: the stale run reached a terminal state
			// and its environment was torn down, so there is no dangling
			// infrastructure to clean up by hand.
			active, err := store.ListNonTerminalRuns(sweepID)
			if err != nil {
				t.Fatalf("ListNonTerminalRuns: %v", err)
			}
			if len(active) != 0 {
				t.Fatalf("ListNonTerminalRuns = %d (%+v), want 0 -- a stale run must not stay in flight", len(active), active)
			}
		})
	}
}

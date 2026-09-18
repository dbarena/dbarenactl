//go:build e2e

package e2e

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbarena/dbarenactl/internal/sweepstate"
)

// liveRunEnv sets up a sweep left stopped with exactly one non-terminal run
// whose environment is still up: a single test point whose benchmark
// succeeds but whose results can never be fetched, so `run` gives up before
// ever reaching teardown (see scenario_fetch_failure_test.go). It is the
// cheapest deterministic way to give `delete` something real to tear down.
// Returns the env and the sweep id.
func liveRunEnv(t *testing.T, workload string, b behavior) (*testEnv, string) {
	t.Helper()
	point := testPointSpec{Tier: "small", BoundType: "io"}
	manifestPath := writeManifest(t, workload, []testPointSpec{point})

	env := newEnv(t, fakeConfig{
		BootstrapDuration: "50ms",
		TeardownDuration:  "50ms",
		TestPoints:        map[string]behavior{"small/io": b},
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
		t.Fatalf("dbarenactl run: exit code 0, want nonzero (the sweep should stop with its environment up)\nstdout:\n%s", res.Stdout)
	}

	sweepID := sweepIDFor(t, manifestPath, workload, iterations, 0)
	active, err := openStore(t, env.dbHome).ListNonTerminalRuns(sweepID)
	if err != nil {
		t.Fatalf("ListNonTerminalRuns: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("ListNonTerminalRuns = %d, want 1 (setup should leave exactly one live environment)", len(active))
	}
	return env, sweepID
}

// tornDown reports whether the fake's own state for runID (standing in for
// benchctl's) shows the environment actually came down.
func tornDown(t *testing.T, stateDir, runID string) bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stateDir, runID+".json"))
	if err != nil {
		t.Fatalf("read fake state for %s: %v", runID, err)
	}
	var st struct {
		Phases       map[string]string `json:"phases"`
		TerminatedAt *string           `json:"terminated_at"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("parse fake state: %v", err)
	}
	return st.Phases["teardown"] == "completed" && st.TerminatedAt != nil
}

// TestDelete_ConfirmDeletesLiveEnvironment covers the main path: a sweep
// with an environment still up is torn down and removed once the user
// confirms.
func TestDelete_ConfirmDeletesLiveEnvironment(t *testing.T) {
	env, sweepID := liveRunEnv(t, "delete-confirm", behavior{Outcome: "success", WorkloadDuration: "50ms", FetchFails: true})
	store := openStore(t, env.dbHome)

	active, err := store.ListNonTerminalRuns(sweepID)
	if err != nil {
		t.Fatalf("ListNonTerminalRuns: %v", err)
	}
	runID := active[0].RunID

	res := runDbarenactl(t, env.vars(), "y\n", "delete", sweepID)
	if res.ExitCode != 0 {
		t.Fatalf("dbarenactl delete: exit code %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "still has 1 running environment(s)") {
		t.Errorf("stderr should warn about the running environment, got:\n%s", res.Stderr)
	}
	// `delete` blocks for minutes on a real teardown, so both the status
	// check and the teardown itself must report progress rather than leaving
	// the user staring at a dead terminal.
	if !strings.Contains(res.Stderr, "Checking 1 environment(s)") {
		t.Errorf("stderr should report the status check, got:\n%s", res.Stderr)
	}
	if !strings.Contains(res.Stderr, "torn down in") {
		t.Errorf("stderr should report the completed teardown, got:\n%s", res.Stderr)
	}
	if !tornDown(t, env.stateDir, runID) {
		t.Errorf("run %s: fake state does not show the environment torn down", runID)
	}
	if _, err := store.GetSweep(sweepID); !errors.Is(err, sweepstate.ErrNotFound) {
		t.Errorf("GetSweep after delete = %v, want ErrNotFound", err)
	}
}

// TestDelete_DeclineAbortsWithoutTeardownOrDeletion checks that answering
// no is a true no-op: the environment stays up and the sweep's state is
// exactly as it was.
func TestDelete_DeclineAbortsWithoutTeardownOrDeletion(t *testing.T) {
	env, sweepID := liveRunEnv(t, "delete-decline", behavior{Outcome: "success", WorkloadDuration: "50ms", FetchFails: true})
	store := openStore(t, env.dbHome)

	active, err := store.ListNonTerminalRuns(sweepID)
	if err != nil {
		t.Fatalf("ListNonTerminalRuns: %v", err)
	}
	runID := active[0].RunID

	res := runDbarenactl(t, env.vars(), "n\n", "delete", sweepID)
	if res.ExitCode == 0 {
		t.Fatalf("dbarenactl delete: exit code 0, want nonzero (declining should abort)\nstdout:\n%s", res.Stdout)
	}
	if tornDown(t, env.stateDir, runID) {
		t.Errorf("run %s was torn down, want the environment left up", runID)
	}
	if _, err := store.GetSweep(sweepID); err != nil {
		t.Errorf("GetSweep after declining: %v, want the sweep still present", err)
	}
	stillActive, err := store.ListNonTerminalRuns(sweepID)
	if err != nil {
		t.Fatalf("ListNonTerminalRuns after declining: %v", err)
	}
	if len(stillActive) != 1 || stillActive[0].RunID != runID {
		t.Errorf("ListNonTerminalRuns after declining = %+v, want the same one run", stillActive)
	}
}

// TestDelete_YesFlagSkipsPrompt covers the scripting/CI path: no prompt at
// all, even with a live environment and no stdin to answer from.
func TestDelete_YesFlagSkipsPrompt(t *testing.T) {
	env, sweepID := liveRunEnv(t, "delete-yes", behavior{Outcome: "success", WorkloadDuration: "50ms", FetchFails: true})
	store := openStore(t, env.dbHome)

	active, err := store.ListNonTerminalRuns(sweepID)
	if err != nil {
		t.Fatalf("ListNonTerminalRuns: %v", err)
	}
	runID := active[0].RunID

	res := runDbarenactl(t, env.vars(), "", "delete", "--yes", sweepID)
	if res.ExitCode != 0 {
		t.Fatalf("dbarenactl delete --yes: exit code %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !tornDown(t, env.stateDir, runID) {
		t.Errorf("run %s: fake state does not show the environment torn down", runID)
	}
	if _, err := store.GetSweep(sweepID); !errors.Is(err, sweepstate.ErrNotFound) {
		t.Errorf("GetSweep after delete = %v, want ErrNotFound", err)
	}
}

// TestDelete_NoRunningEnvironmentsNeedsNoPrompt deletes a fully completed
// sweep: nothing is up, so there is nothing to confirm and no stdin is
// needed.
func TestDelete_NoRunningEnvironmentsNeedsNoPrompt(t *testing.T) {
	point := testPointSpec{Tier: "small", BoundType: "io"}
	manifestPath := writeManifest(t, "delete-completed", []testPointSpec{point})

	env := newEnv(t, fakeConfig{
		BootstrapDuration: "50ms",
		TeardownDuration:  "50ms",
		Default:           behavior{Outcome: "success", WorkloadDuration: "50ms"},
	})

	const maxConcurrency = 1
	const iterations = 1

	res := runDbarenactl(t, env.vars(), "",
		"run",
		"--candidate", manifestPath,
		"--max-concurrency", itoa(maxConcurrency),
		"--iterations", itoa(iterations),
	)
	if res.ExitCode != 0 {
		t.Fatalf("dbarenactl run: exit code %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}

	sweepID := sweepIDFor(t, manifestPath, "delete-completed", iterations, 0)
	store := openStore(t, env.dbHome)

	deleteRes := runDbarenactl(t, env.vars(), "", "delete", sweepID)
	if deleteRes.ExitCode != 0 {
		t.Fatalf("dbarenactl delete: exit code %d\nstdout:\n%s\nstderr:\n%s", deleteRes.ExitCode, deleteRes.Stdout, deleteRes.Stderr)
	}
	if _, err := store.GetSweep(sweepID); !errors.Is(err, sweepstate.ErrNotFound) {
		t.Errorf("GetSweep after delete = %v, want ErrNotFound", err)
	}
}

// TestDelete_TeardownFailureIsWarningNotAbort covers an environment that
// refuses to come down: `delete` reports it and removes the sweep anyway,
// rather than leaving state behind for infra that needs manual attention
// regardless.
func TestDelete_TeardownFailureIsWarningNotAbort(t *testing.T) {
	env, sweepID := liveRunEnv(t, "delete-teardown-failure", behavior{Outcome: "success", WorkloadDuration: "50ms", TeardownFails: true})
	store := openStore(t, env.dbHome)

	res := runDbarenactl(t, env.vars(), "", "delete", "--yes", sweepID)
	if res.ExitCode != 0 {
		t.Fatalf("dbarenactl delete --yes: exit code %d, want 0 (a teardown failure is only a warning)\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "tear down failed") {
		t.Errorf("stderr should report the failed teardown, got:\n%s", res.Stderr)
	}
	if _, err := store.GetSweep(sweepID); !errors.Is(err, sweepstate.ErrNotFound) {
		t.Errorf("GetSweep after delete = %v, want ErrNotFound", err)
	}
}

// TestDelete_RunAlreadyGoneFromBenchctlNeedsNoPrompt reproduces a run whose
// environment benchctl no longer has any record of at all -- e.g. someone
// ran `benchctl teardown --purge` on it by hand. dbarenactl's local
// bookkeeping still shows it non-terminal, but `delete` must not treat that
// alone as "still running": it should skip the confirmation prompt and the
// teardown call for that run, and still delete the sweep.
func TestDelete_RunAlreadyGoneFromBenchctlNeedsNoPrompt(t *testing.T) {
	env, sweepID := liveRunEnv(t, "delete-purged", behavior{Outcome: "success", WorkloadDuration: "50ms", FetchFails: true})
	store := openStore(t, env.dbHome)

	active, err := store.ListNonTerminalRuns(sweepID)
	if err != nil {
		t.Fatalf("ListNonTerminalRuns: %v", err)
	}
	runID := active[0].RunID

	// Simulate benchctl having purged its own record of this run: the fake
	// reports "no local state found" for it, exactly like the real benchctl
	// does for a run it has never heard of.
	if err := os.Remove(filepath.Join(env.stateDir, runID+".json")); err != nil {
		t.Fatalf("remove fake state for %s: %v", runID, err)
	}

	res := runDbarenactl(t, env.vars(), "", "delete", sweepID)
	if res.ExitCode != 0 {
		t.Fatalf("dbarenactl delete: exit code %d, want 0 (nothing is actually running, so no prompt should block it)\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if strings.Contains(res.Stderr, "running environment") {
		t.Errorf("stderr should not warn about a running environment once benchctl has no record of it, got:\n%s", res.Stderr)
	}
	if _, err := store.GetSweep(sweepID); !errors.Is(err, sweepstate.ErrNotFound) {
		t.Errorf("GetSweep after delete = %v, want ErrNotFound", err)
	}
}

// TestDelete_NonexistentSweepErrors checks that deleting an unknown sweep
// fails loudly instead of silently succeeding.
func TestDelete_NonexistentSweepErrors(t *testing.T) {
	env := newEnv(t, fakeConfig{Default: behavior{Outcome: "success", WorkloadDuration: "50ms"}})

	res := runDbarenactl(t, env.vars(), "", "delete", "some-made-up-sweep-id")
	if res.ExitCode == 0 {
		t.Fatalf("dbarenactl delete: exit code 0, want nonzero\nstdout:\n%s\nstderr:\n%s", res.Stdout, res.Stderr)
	}
}

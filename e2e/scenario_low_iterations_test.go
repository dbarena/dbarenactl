//go:build e2e

package e2e

import (
	"errors"
	"strings"
	"testing"

	"github.com/dbarena/dbarenactl/internal/sweepstate"
)

// TestRun_IterationsIsRequired checks that `run` refuses to start without an
// explicit --iterations.
func TestRun_IterationsIsRequired(t *testing.T) {
	manifestPath := writeManifest(t, "iterations-required", []testPointSpec{{Tier: "small", BoundType: "io"}})
	env := newEnv(t, fakeConfig{Default: behavior{Outcome: "success", WorkloadDuration: "50ms"}})

	res := runDbarenactl(t, env.vars(), "",
		"run",
		"--candidate", manifestPath,
	)
	if res.ExitCode == 0 {
		t.Fatalf("dbarenactl run: exit code 0, want non-zero\nstdout:\n%s\nstderr:\n%s", res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stderr, `required flag(s) "iterations" not set`) {
		t.Errorf("stderr should report the missing --iterations flag:\n%s", res.Stderr)
	}
}

// TestRun_LowIterationsDeclined_CreatesNoSweep checks that answering "no" to
// the below-minimum warning aborts before any sweep state is created.
func TestRun_LowIterationsDeclined_CreatesNoSweep(t *testing.T) {
	manifestPath := writeManifest(t, "low-iterations-declined", []testPointSpec{{Tier: "small", BoundType: "io"}})
	env := newEnv(t, fakeConfig{Default: behavior{Outcome: "success", WorkloadDuration: "50ms"}})

	const iterations = 2
	res := runDbarenactl(t, env.vars(), "n\n",
		"run",
		"--candidate", manifestPath,
		"--iterations", itoa(iterations),
	)
	if res.ExitCode == 0 {
		t.Fatalf("dbarenactl run: exit code 0, want non-zero\nstdout:\n%s\nstderr:\n%s", res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "requires at least 3 iterations. Proceed anyway?") {
		t.Errorf("stderr should warn about --iterations below 3:\n%s", res.Stderr)
	}

	sweepID := sweepIDFor(t, manifestPath, "low-iterations-declined", iterations, 0)
	store := openStore(t, env.dbHome)
	if _, err := store.GetSweep(sweepID); !errors.Is(err, sweepstate.ErrNotFound) {
		t.Errorf("GetSweep = %v, want %v (no sweep should have been created)", err, sweepstate.ErrNotFound)
	}
}

// TestRun_LowIterationsDryRun_WarnsWithoutAsking checks that a dry run below
// the minimum warns but succeeds without reading an answer.
func TestRun_LowIterationsDryRun_WarnsWithoutAsking(t *testing.T) {
	manifestPath := writeManifest(t, "low-iterations-dry-run", []testPointSpec{{Tier: "small", BoundType: "io"}})
	env := newEnv(t, fakeConfig{Default: behavior{Outcome: "success", WorkloadDuration: "50ms"}})

	res := runDbarenactl(t, env.vars(), "",
		"run",
		"--candidate", manifestPath,
		"--iterations", "2",
		"--dry-run",
	)
	if res.ExitCode != 0 {
		t.Fatalf("dbarenactl run --dry-run: exit code %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "warning: dbarena's benchmark methodology requires at least 3 iterations.") {
		t.Errorf("stderr should warn about --iterations below 3:\n%s", res.Stderr)
	}
	if strings.Contains(res.Stderr, "Proceed anyway?") {
		t.Errorf("a dry run must not ask for confirmation:\n%s", res.Stderr)
	}
}

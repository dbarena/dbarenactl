//go:build e2e

package e2e

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbarena/dbarenactl/internal/sweepstate"
)

// TestRun_MissingBenchctl_FailsBeforeCreatingSweep covers the case where
// benchctl can't be executed at all (missing from PATH, bad --benchctl-bin,
// not executable). Unlike a launch failure discovered mid-sweep (see
// scenario_launch_failure_test.go), this must be caught before `run` creates
// any sweep state at all -- otherwise the user is left with a sweep that's
// already dead on arrival and has to `resume` or `delete` it before they can
// just fix benchctl and re-run cleanly.
func TestRun_MissingBenchctl_FailsBeforeCreatingSweep(t *testing.T) {
	point := testPointSpec{Tier: "small", BoundType: "io"}
	manifestPath := writeManifest(t, "missing-benchctl", []testPointSpec{point})

	env := newEnv(t, fakeConfig{})
	noSuchBenchctl := filepath.Join(t.TempDir(), "no-such-benchctl")

	const maxConcurrency = 1
	const iterations = 1

	res := runDbarenactlBin(t, noSuchBenchctl, env.vars(), "",
		"run",
		"--candidate", manifestPath,
		"--max-concurrency", itoa(maxConcurrency),
		"--iterations", itoa(iterations),
	)
	if res.ExitCode == 0 {
		t.Fatalf("dbarenactl run: exit code 0, want nonzero (benchctl is unusable)\nstdout:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "benchctl binary could not be executed") {
		t.Errorf("stderr = %q, want it to mention the benchctl binary being unusable", res.Stderr)
	}
	if strings.Contains(res.Stderr, "dbarenactl resume") {
		t.Errorf("stderr = %q, should not suggest `dbarenactl resume` -- no sweep was ever created", res.Stderr)
	}

	sweepID := sweepIDFor(t, manifestPath, "missing-benchctl", iterations, 0)
	store := openStore(t, env.dbHome)
	if _, err := store.GetSweep(sweepID); !errors.Is(err, sweepstate.ErrNotFound) {
		t.Errorf("GetSweep = %v, want %v (no sweep should have been created)", err, sweepstate.ErrNotFound)
	}
}

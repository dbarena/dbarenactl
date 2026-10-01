//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// globPoints is the fixture both scenarios below sweep over: one bound type
// spread across two tiers and two variants, so a `*cache-fit*` pattern has to
// cross "/" in several places to select the right subset.
var globPoints = []testPointSpec{
	{Tier: "small", BoundType: "cache-fit"},
	{Tier: "small", BoundType: "cache-exceeding"},
	{Tier: "2xlarge", BoundType: "cache-fit", Variant: "performance-optimized"},
	{Tier: "2xlarge", BoundType: "cache-fit", Variant: "cost-optimized"},
}

// TestTestPointGlob_DryRunPreviewsEveryMatch checks that a --test-point glob
// selects every matching test point and nothing else, and that the CLI says
// which ones it matched. Dry run persists nothing, so this needs no sweep
// state.
func TestTestPointGlob_DryRunPreviewsEveryMatch(t *testing.T) {
	manifestPath := writeManifest(t, "test-point-glob-dry-run", globPoints)
	env := newEnv(t, fakeConfig{Default: behavior{Outcome: "success", WorkloadDuration: "50ms"}})

	res := runDbarenactl(t, env.vars(), "",
		"run",
		"--candidate", manifestPath,
		"--iterations", "3",
		"--test-point", "*cache-fit*",
		"--dry-run",
	)
	if res.ExitCode != 0 {
		t.Fatalf("dbarenactl run --dry-run: exit code %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}

	// printDryRun emits one "[<key>]" block per selected test point.
	for _, want := range []string{
		"[small/cache-fit]",
		"[2xlarge/cache-fit/performance-optimized]",
		"[2xlarge/cache-fit/cost-optimized]",
	} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("dry-run stdout missing %s:\n%s", want, res.Stdout)
		}
	}
	if strings.Contains(res.Stdout, "cache-exceeding") {
		t.Errorf("dry-run stdout covers cache-exceeding, which the pattern excludes:\n%s", res.Stdout)
	}

	if !strings.Contains(res.Stderr, "matched 3 test point(s)") {
		t.Errorf("stderr missing the match summary:\n%s", res.Stderr)
	}
}

// TestTestPointGlob_SweepsOnlyMatchedTestPoints drives a real sweep scoped by
// a glob: the sweep must contain exactly the matched test points, all run to
// completion.
func TestTestPointGlob_SweepsOnlyMatchedTestPoints(t *testing.T) {
	const workload = "test-point-glob"
	const iterations = 1
	const scope = "*cache-fit*"

	manifestPath := writeManifest(t, workload, globPoints)
	env := newEnv(t, fakeConfig{Default: behavior{Outcome: "success", WorkloadDuration: "50ms"}})

	res := runDbarenactl(t, env.vars(), "y\n",
		"run",
		"--candidate", manifestPath,
		"--test-point", scope,
		"--iterations", itoa(iterations),
	)
	if res.ExitCode != 0 {
		t.Fatalf("dbarenactl run: exit code %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}

	sweepID := sweepIDForScope(t, manifestPath, workload, iterations, 0, scope)
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
	got := map[string]bool{}
	for _, tp := range testPoints {
		got[tp.Label()] = true
		if tp.SuccessesCount != iterations {
			t.Errorf("test point %s: SuccessesCount = %d, want %d", tp.ID, tp.SuccessesCount, iterations)
		}
	}
	want := []string{"small/cache-fit", "2xlarge/cache-fit/performance-optimized", "2xlarge/cache-fit/cost-optimized"}
	if len(got) != len(want) {
		t.Fatalf("sweep covers %d test points (%v), want %d", len(got), got, len(want))
	}
	for _, label := range want {
		if !got[label] {
			t.Errorf("sweep is missing test point %s (has %v)", label, got)
		}
	}
}

// TestTestPointGlob_NoMatchListsAvailable checks the failure mode: a pattern
// matching nothing must fail up front and name the manifest's real keys.
func TestTestPointGlob_NoMatchListsAvailable(t *testing.T) {
	manifestPath := writeManifest(t, "test-point-glob-no-match", globPoints)
	env := newEnv(t, fakeConfig{Default: behavior{Outcome: "success", WorkloadDuration: "50ms"}})

	res := runDbarenactl(t, env.vars(), "",
		"run",
		"--candidate", manifestPath,
		"--iterations", "3",
		"--test-point", "nope*",
		"--dry-run",
	)
	if res.ExitCode == 0 {
		t.Fatalf("dbarenactl run: exit code 0, want non-zero\nstdout:\n%s\nstderr:\n%s", res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "no matching test point") || !strings.Contains(res.Stderr, "small/cache-fit") {
		t.Errorf("stderr should report no match and list available keys:\n%s", res.Stderr)
	}
}

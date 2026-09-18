//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// runTimeline is the subset of a fake run's recorded state that says when its
// environment existed and when it stopped being worth paying for.
type runTimeline struct {
	RunID        string     `json:"run_id"`
	StartedAt    *time.Time `json:"started_at"`
	CompletedAt  *time.Time `json:"completed_at"`
	TerminatedAt *time.Time `json:"terminated_at"`
}

func readTimelines(t *testing.T, stateDir string) []runTimeline {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(stateDir, "*.json"))
	if err != nil {
		t.Fatalf("glob state dir: %v", err)
	}
	var out []runTimeline
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			t.Fatalf("read %s: %v", m, err)
		}
		var rt runTimeline
		if err := json.Unmarshal(data, &rt); err != nil {
			continue // not a run state file
		}
		if rt.RunID != "" {
			out = append(out, rt)
		}
	}
	return out
}

// TestPromptTeardown_NothingLaunchesWhileAFinishedEnvironmentIsStillUp is the
// end-to-end guard on the sweep that left eight finished environments billing
// for up to 87 minutes each. Provisioning is made slow relative to the
// workload, which is what made the old greedy fill visible: it launched every
// concurrency slot back to back, blocking on each provision, and fetched and
// tore nothing down until the last one returned.
//
// Rather than assert on wall-clock idle time, which would be flaky, this
// checks the ordering the cost invariant actually promises: no environment is
// ever launched while another has already finished and is still up.
func TestPromptTeardown_NothingLaunchesWhileAFinishedEnvironmentIsStillUp(t *testing.T) {
	points := []testPointSpec{
		{Tier: "small", BoundType: "io"},
		{Tier: "small", BoundType: "compute"},
		{Tier: "medium", BoundType: "io"},
	}
	manifestPath := writeManifest(t, "prompt-teardown", points)

	env := newEnv(t, fakeConfig{
		// A provision that dwarfs the workload: every environment finishes
		// its work while the next one is still coming up.
		BootstrapDuration: "400ms",
		Default:           behavior{Outcome: "success", WorkloadDuration: "20ms"},
	})

	const iterations = 2
	// More slots than the sweep can usefully fill at once, so nothing but the
	// scheduler's own policy limits how many environments are alive.
	res := runDbarenactl(t, env.vars(), "",
		"run",
		"--candidate", manifestPath,
		"--max-concurrency", "6",
		"--iterations", itoa(iterations),
	)
	if res.ExitCode != 0 {
		t.Fatalf("dbarenactl run: exit code %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}

	timelines := readTimelines(t, env.stateDir)
	if len(timelines) != len(points)*iterations {
		t.Fatalf("got %d runs, want %d", len(timelines), len(points)*iterations)
	}

	for _, launched := range timelines {
		if launched.StartedAt == nil {
			t.Fatalf("run %s has no started_at", launched.RunID)
		}
		for _, other := range timelines {
			if other.RunID == launched.RunID || other.CompletedAt == nil {
				continue
			}
			// other was finished -- producing nothing, still billing -- at
			// the moment launched started provisioning.
			if other.CompletedAt.Before(*launched.StartedAt) &&
				(other.TerminatedAt == nil || other.TerminatedAt.After(*launched.StartedAt)) {
				t.Errorf("launched %s at %s while %s had already finished at %s and was not torn down until %v",
					launched.RunID, launched.StartedAt.Format(time.RFC3339Nano),
					other.RunID, other.CompletedAt.Format(time.RFC3339Nano), other.TerminatedAt)
			}
		}
	}

	for _, rt := range timelines {
		if rt.TerminatedAt == nil {
			t.Errorf("run %s was never torn down", rt.RunID)
		}
	}
}

package planner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbarena/dbarenactl/internal/manifest"
)

const testManifestYAML = `
provider: aws/rds
workload: tpcc
scenario_path: rds-tpcc-ec2-tiers.yaml
test_points:
  - tier: small
    bound_type: io
    set: {project_size: small, disk_iops: "3000"}
  - tier: small
    bound_type: compute
    set: {project_size: small, disk_iops: "0"}
`

func testManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aws-rds.yaml")
	if err := os.WriteFile(path, []byte(testManifestYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return m
}

func TestBuildTestPoints(t *testing.T) {
	m := testManifest(t)
	tps := BuildTestPoints("aws-rds-abc123", m, 5, 5)

	if len(tps) != 2 {
		t.Fatalf("len(tps) = %d, want 2", len(tps))
	}
	for _, tp := range tps {
		if tp.SweepID != "aws-rds-abc123" {
			t.Errorf("SweepID = %q", tp.SweepID)
		}
		if tp.Workload != "tpcc" || tp.Scenario == "" {
			t.Errorf("tp = %+v", tp)
		}
		if tp.SuccessesNeeded != 5 || tp.FailureBudget != 5 {
			t.Errorf("tp budgets = %+v", tp)
		}
	}
	if tps[0].ID == tps[1].ID {
		t.Error("test points should get distinct ids")
	}
	if !strings.HasPrefix(tps[0].ID, "aws-rds-abc123-small-io") {
		t.Errorf("id = %q", tps[0].ID)
	}
}

func TestBuildTestPoints_SameManifestSameSweepIDIsDeterministic(t *testing.T) {
	m := testManifest(t)
	a := BuildTestPoints("sweep-x", m, 5, 5)
	b := BuildTestPoints("sweep-x", m, 5, 5)
	for i := range a {
		if a[i].ID != b[i].ID {
			t.Errorf("ids differ across calls: %q != %q", a[i].ID, b[i].ID)
		}
	}
}

func TestNewRunID_UniqueAcrossCalls(t *testing.T) {
	a := NewRunID("tp-1", 1)
	b := NewRunID("tp-1", 1)
	if a == b {
		t.Error("NewRunID should not produce the same id twice, even for the same test point/attempt")
	}
	if !strings.HasPrefix(a, "tp-1-1-") {
		t.Errorf("a = %q", a)
	}
}

func TestLaunchCommand_String(t *testing.T) {
	c := LaunchCommand{
		RunID:        "run-1",
		ScenarioPath: "scenarios/x.yaml",
		Set:          map[string]string{"b": "2", "a": "1"},
	}
	want := "benchctl run --async --run-id run-1 scenarios/x.yaml --set a=1 --set b=2"
	if got := c.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestPreviewFirstAttempts(t *testing.T) {
	m := testManifest(t)
	cmds := PreviewFirstAttempts("sweep-x", m)
	if len(cmds) != 2 {
		t.Fatalf("len(cmds) = %d, want 2", len(cmds))
	}
	for _, c := range cmds {
		if !strings.Contains(c.String(), "benchctl run --async --run-id sweep-x-") {
			t.Errorf("unexpected command: %s", c.String())
		}
	}
}

//go:build e2e

// Package e2e drives the real, compiled dbarenactl binary as a subprocess
// against the fake benchctl in e2e/fakebenchctl, covering the scenarios a
// unit test can't: real blocking bootstrap/teardown, a genuinely detached
// async workload, and dbarenactl's own process-level behavior (exit codes,
// the interactive resume prompt, its on-disk state surviving across
// separate invocations).
//
// Run with `mise run test-e2e` (equivalently `go test -tags e2e ./e2e/...`).
// Excluded from the default `go test ./...` / `mise run test` on purpose --
// these tests spawn real processes and take real wall-clock time.
package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dbarena/dbarenactl/internal/manifest"
	"github.com/dbarena/dbarenactl/internal/planner"
	"github.com/dbarena/dbarenactl/internal/sweepid"
	"github.com/dbarena/dbarenactl/internal/sweepstate"
)

var (
	dbarenactlBin   string
	fakeBenchctlBin string
)

// TestMain builds the two binaries the whole suite shares once, rather than
// once per test.
func TestMain(m *testing.M) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: go env GOMOD: %v\n", err)
		os.Exit(1)
	}
	repoRoot := filepath.Dir(strings.TrimSpace(string(out)))

	buildDir, err := os.MkdirTemp("", "dbarenactl-e2e-bin-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(buildDir)

	dbarenactlBin = filepath.Join(buildDir, "dbarenactl")
	fakeBenchctlBin = filepath.Join(buildDir, "fakebenchctl")

	if err := goBuild(repoRoot, dbarenactlBin, "./cmd/dbarenactl"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: build dbarenactl: %v\n", err)
		os.Exit(1)
	}
	if err := goBuild(repoRoot, fakeBenchctlBin, "./e2e/fakebenchctl"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: build fakebenchctl: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

func goBuild(dir, out, pkg string) error {
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, stderr.String())
	}
	return nil
}

// behavior/config mirrors e2e/fakebenchctl's own Config/Behavior so
// scenario tests build a config as data, not hand-written JSON.
type behavior struct {
	Outcome          string `json:"outcome,omitempty"`
	WorkloadDuration string `json:"workload_duration,omitempty"`
	LaunchFails      bool   `json:"launch_fails,omitempty"`
	FetchFails       bool   `json:"fetch_fails,omitempty"`
	TeardownFails    bool   `json:"teardown_fails,omitempty"`
	StaleHeartbeat   bool   `json:"stale_heartbeat,omitempty"`
}

type fakeConfig struct {
	BootstrapDuration string              `json:"bootstrap_duration,omitempty"`
	TeardownDuration  string              `json:"teardown_duration,omitempty"`
	Default           behavior            `json:"default"`
	TestPoints        map[string]behavior `json:"test_points"`
}

// testEnv is one scenario's isolated sandbox: its own dbarenactl state
// directory and fake benchctl state directory, so scenarios never interfere
// with each other or with a real ~/.dbarenactl.
type testEnv struct {
	dbHome     string
	stateDir   string
	configPath string
}

// newEnv allocates a sandbox and writes cfg for the fake to read. It
// registers a cleanup that kills any fakebenchctl worker process a failed
// or timed-out test left running -- a real risk here, since scenarios
// deliberately leave some runs mid-flight.
func newEnv(t *testing.T, cfg fakeConfig) *testEnv {
	t.Helper()
	e := &testEnv{
		dbHome:     t.TempDir(),
		stateDir:   t.TempDir(),
		configPath: filepath.Join(t.TempDir(), "fakebench-config.json"),
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal fake config: %v", err)
	}
	if err := os.WriteFile(e.configPath, data, 0o644); err != nil {
		t.Fatalf("write fake config: %v", err)
	}
	t.Cleanup(func() { killLeftoverWorkers(t, e.stateDir) })
	return e
}

// vars are the environment variables every dbarenactl/fakebenchctl
// invocation in a scenario needs. DBARENACTL_POLL_INTERVAL keeps the
// scheduler's poll loop fast enough for the whole suite to run in seconds
// instead of tens of minutes (the real default is 30s).
func (e *testEnv) vars() map[string]string {
	return map[string]string{
		"DBARENACTL_HOME":          e.dbHome,
		"FAKEBENCH_STATE_DIR":      e.stateDir,
		"FAKEBENCH_CONFIG":         e.configPath,
		"DBARENACTL_POLL_INTERVAL": "50ms",
	}
}

func killLeftoverWorkers(t *testing.T, stateDir string) {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(stateDir, "*.pid"))
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			continue
		}
		proc, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		if proc.Signal(syscall.Signal(0)) == nil {
			t.Logf("killing leftover fakebenchctl worker pid %d (%s)", pid, filepath.Base(m))
			_ = proc.Kill()
		}
	}
}

// testPointSpec is one manifest test point, kept minimal -- these fixtures
// never need disk/param overrides, just enough shape for the fake to match
// behavior against (see e2e/fakebenchctl's testPointSlug).
type testPointSpec struct {
	Tier, BoundType, Variant string
}

// writeManifest writes a minimal valid manifest (see
// internal/manifest.Manifest.validate) to a temp file and returns its path.
// scenario_path is a placeholder -- the fake never reads it. Provider/Product
// are fixed to AWS/RDS -- these are synthetic fixtures against a fake
// benchctl, not real cloud providers, and internal/manifest's Provider enum
// no longer accepts arbitrary strings -- so each test's distinguishing label
// (formerly passed as an opaque "provider" string) is now carried in
// workload instead, which also feeds internal/sweepid's hash.
func writeManifest(t *testing.T, workload string, points []testPointSpec) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "provider: AWS\nproduct: RDS\nworkload: %s\nscenario_path: placeholder-scenario.yaml\ntest_points:\n", workload)
	for _, p := range points {
		fmt.Fprintf(&b, "  - tier: %s\n    bound_type: %s\n", p.Tier, p.BoundType)
		if p.Variant != "" {
			fmt.Fprintf(&b, "    variant: %s\n", p.Variant)
		}
		fmt.Fprintln(&b, "    set: {}")
	}
	path := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

// sweepIDFor recomputes the sweep id `dbarenactl run` would derive for
// manifestPath under these flags, exactly as cmd/dbarenactl/run.go does --
// letting tests find their sweep's rows without scraping CLI output.
// Provider/Product are fixed to AWS/RDS, matching writeManifest's fixed
// combo. Concurrency isn't part of a sweep's identity (see internal/sweepid),
// so it's not a parameter here either.
func sweepIDFor(t *testing.T, manifestPath, workload string, iterations, maxWorkloadFailures int) string {
	t.Helper()
	content, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	return sweepid.Compute(sweepid.Params{
		Provider:            "AWS",
		Product:             "RDS",
		Workload:            workload,
		ManifestContent:     content,
		Iterations:          iterations,
		OnWorkloadFailure:   "retry",
		MaxWorkloadFailures: maxWorkloadFailures,
	})
}

// testPointID recomputes the row id planner.BuildTestPoints would assign a
// given tier/bound_type/variant within sweepID.
func testPointID(sweepID string, p testPointSpec) string {
	return planner.TestPointID(sweepID, manifest.TestPointDef{
		Tier: p.Tier, BoundType: p.BoundType, Variant: p.Variant,
	})
}

// itoa exists only to keep scenario files' cobra-flag-value arguments
// (always strings) readable at call sites.
func itoa(n int) string { return strconv.Itoa(n) }

type runResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// runDbarenactl runs the built dbarenactl binary as a real subprocess.
// stdin is used for resume's continue/restart prompt and delete's
// confirmation prompt; pass "" when not needed.
// Every subcommand this suite drives (run/resume/status/delete) accepts
// --benchctl-bin; runDbarenactl always points it at the fake so callers
// don't have to repeat it.
func runDbarenactl(t *testing.T, env map[string]string, stdin string, args ...string) runResult {
	t.Helper()
	return runDbarenactlBin(t, fakeBenchctlBin, env, stdin, args...)
}

// runDbarenactlBin is runDbarenactl with an explicit --benchctl-bin, for
// scenarios (e.g. a missing/unusable binary) that need something other than
// the fake.
func runDbarenactlBin(t *testing.T, benchctlBin string, env map[string]string, stdin string, args ...string) runResult {
	t.Helper()
	args = append(args, "--benchctl-bin="+benchctlBin)
	cmd := exec.Command(dbarenactlBin, args...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("run dbarenactl %v: %v", args, err)
		}
	}
	return runResult{stdout.String(), stderr.String(), exitCode}
}

// openStore opens the sqlite state file a prior dbarenactl invocation under
// dbHome left behind, so assertions can check exact rows instead of
// scraping CLI text.
func openStore(t *testing.T, dbHome string) *sweepstate.Store {
	t.Helper()
	store, err := sweepstate.Open(filepath.Join(dbHome, "dbarenactl.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// waitForWorkloadCompletion polls a run's fake state file until benchctl
// (per the fake) reports the workload finished, without touching the
// process running it -- used by the budget-exhaustion scenario to let a
// deliberately-not-killed worker finish on its own before resuming.
func waitForWorkloadCompletion(t *testing.T, stateDir, runID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(stateDir, runID+".json"))
		if err == nil && strings.Contains(string(data), `"completed_at"`) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s did not complete within %s", runID, timeout)
}

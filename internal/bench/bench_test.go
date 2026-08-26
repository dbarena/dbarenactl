package bench

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeBenchctl writes an executable shell script standing in for the real
// benchctl binary. body receives "$@" as its argument list and should exit
// nonzero (with a message on stderr) to simulate failure.
func fakeBenchctl(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "benchctl")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake benchctl: %v", err)
	}
	return path
}

func TestLaunchAsync_BuildsExpectedArgs(t *testing.T) {
	bin := fakeBenchctl(t, `echo "$@" > `+os.TempDir()+`/dbarenactl-bench-test-args.txt`)
	c := New(bin)

	err := c.LaunchAsync(context.Background(), "run-1", "scenarios/x.yaml", map[string]string{
		"project_size": "small",
		"disk_iops":    "3000",
	})
	if err != nil {
		t.Fatalf("LaunchAsync: %v", err)
	}

	got, err := os.ReadFile(os.TempDir() + "/dbarenactl-bench-test-args.txt")
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(got))
	want := "run --async --run-id run-1 scenarios/x.yaml --set disk_iops=3000 --set project_size=small"
	if line != want {
		t.Errorf("args = %q, want %q", line, want)
	}
}

func TestLaunchAsync_PropagatesFailure(t *testing.T) {
	bin := fakeBenchctl(t, `echo "boom: AWS credentials expired" >&2; exit 1`)
	c := New(bin)

	err := c.LaunchAsync(context.Background(), "run-1", "scenarios/x.yaml", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "AWS credentials expired") {
		t.Errorf("error %q should include the subprocess's stderr", err)
	}
}

func TestLaunchAsync_WithLogDirWritesPerRunLogFile(t *testing.T) {
	bin := fakeBenchctl(t, `echo "provisioning..."; echo "warning: slow network" >&2`)
	c := New(bin)
	c.LogDir = t.TempDir()

	if err := c.LaunchAsync(context.Background(), "run-1", "scenarios/x.yaml", nil); err != nil {
		t.Fatalf("LaunchAsync: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(c.LogDir, "run-1.log"))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	for _, want := range []string{"run --async --run-id run-1 scenarios/x.yaml", "provisioning...", "warning: slow network"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("log file = %q, want it to contain %q", got, want)
		}
	}
}

func TestLaunchAsync_WithLogDirFailure_ErrorPointsAtLogFileInsteadOfEmbeddingStderr(t *testing.T) {
	bin := fakeBenchctl(t, `echo "boom: AWS credentials expired" >&2; exit 1`)
	c := New(bin)
	c.LogDir = t.TempDir()

	err := c.LaunchAsync(context.Background(), "run-1", "scenarios/x.yaml", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	wantLog := filepath.Join(c.LogDir, "run-1.log")
	if !strings.Contains(err.Error(), wantLog) {
		t.Errorf("error %q should point at %q", err, wantLog)
	}
	if strings.Contains(err.Error(), "AWS credentials expired") {
		t.Errorf("error %q should not embed the full stderr once a log file is configured", err)
	}

	got, err := os.ReadFile(wantLog)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(got), "AWS credentials expired") {
		t.Errorf("log file = %q, want it to contain the full stderr", got)
	}
}

func TestLaunchAsync_WithLogDirLogFileStreamsWhileSubprocessStillRunning(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	bin := fakeBenchctl(t, `echo "provisioning..."; touch `+marker+`; sleep 5`)
	c := New(bin)
	c.LogDir = t.TempDir()

	launchDone := make(chan error, 1)
	go func() {
		launchDone <- c.LaunchAsync(context.Background(), "run-1", "scenarios/x.yaml", nil)
	}()

	logPath := filepath.Join(c.LogDir, "run-1.log")
	deadline := time.Now().Add(4 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake benchctl never reported starting up")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The subprocess is now sleeping, well before it exits. The log file
	// must already exist and contain its output -- proof this isn't
	// buffered in memory until the process finishes.
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("log file should exist while the subprocess is still running: %v", err)
	}
	if !strings.Contains(string(got), "provisioning...") {
		t.Errorf("log file = %q, want it to already contain live output", got)
	}

	if err := <-launchDone; err != nil {
		t.Fatalf("LaunchAsync: %v", err)
	}
}

func TestStatus_WithLogDirConfigured_NeverWritesLogFile(t *testing.T) {
	bin := fakeBenchctl(t, `cat <<'EOF'
{"run_id":"run-1","phases":{"provision":"completed"}}
EOF`)
	c := New(bin)
	c.LogDir = t.TempDir()

	if _, err := c.Status(context.Background(), "run-1"); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.LogDir, "run-1.log")); !os.IsNotExist(err) {
		t.Errorf("expected no log file for a Status call, stat err = %v", err)
	}
}

func TestStatus_ParsesJSON(t *testing.T) {
	bin := fakeBenchctl(t, `cat <<'EOF'
{"run_id":"run-1","phases":{"provision":"completed","driver.setup":"running"}}
EOF`)
	c := New(bin)

	st, err := c.Status(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.RunID != "run-1" || st.Phases["provision"] != "completed" {
		t.Errorf("st = %+v", st)
	}
}

func TestStatus_NotFoundLocalStore(t *testing.T) {
	bin := fakeBenchctl(t, `echo 'Error: runstate: no local state found for run "run-1"; for remote runs set BENCHCTL_STORE_URL' >&2; exit 1`)
	c := New(bin)

	_, err := c.Status(context.Background(), "run-1")
	if !errors.Is(err, ErrRunNotFound) {
		t.Errorf("err = %v, want ErrRunNotFound", err)
	}
}

func TestStatus_NotFoundRemoteStore(t *testing.T) {
	bin := fakeBenchctl(t, `echo 'Error: runstate: run "run-1" not found' >&2; exit 1`)
	c := New(bin)

	_, err := c.Status(context.Background(), "run-1")
	if !errors.Is(err, ErrRunNotFound) {
		t.Errorf("err = %v, want ErrRunNotFound", err)
	}
}

func TestStatus_OtherFailureNotMaskedAsNotFound(t *testing.T) {
	bin := fakeBenchctl(t, `echo 'Error: runstate: supabase load: HTTP 500: internal error' >&2; exit 1`)
	c := New(bin)

	_, err := c.Status(context.Background(), "run-1")
	if errors.Is(err, ErrRunNotFound) {
		t.Error("a 500 should not be reported as ErrRunNotFound")
	}
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestStatus_MissingBinary_ReturnsErrBenchctlUnusable(t *testing.T) {
	c := New(filepath.Join(t.TempDir(), "no-such-benchctl"))

	_, err := c.Status(context.Background(), "run-1")
	if !errors.Is(err, ErrBenchctlUnusable) {
		t.Errorf("err = %v, want ErrBenchctlUnusable", err)
	}
}

func TestStatus_NonExecutableBinary_ReturnsErrBenchctlUnusable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "benchctl")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil {
		t.Fatalf("write non-executable benchctl: %v", err)
	}
	c := New(path)

	_, err := c.Status(context.Background(), "run-1")
	if !errors.Is(err, ErrBenchctlUnusable) {
		t.Errorf("err = %v, want ErrBenchctlUnusable", err)
	}
}

func TestStatus_DomainErrorNotMisclassifiedAsUnusable(t *testing.T) {
	bin := fakeBenchctl(t, `echo 'Error: runstate: supabase load: HTTP 500: internal error' >&2; exit 1`)
	c := New(bin)

	_, err := c.Status(context.Background(), "run-1")
	if errors.Is(err, ErrBenchctlUnusable) {
		t.Error("a real benchctl invocation that ran and failed should not be classified as ErrBenchctlUnusable")
	}
}

func TestFetch_BuildsExpectedArgsAndPropagatesFailure(t *testing.T) {
	bin := fakeBenchctl(t, `echo "$@" > `+os.TempDir()+`/dbarenactl-bench-fetch-args.txt`)
	c := New(bin)
	if err := c.Fetch(context.Background(), "run-1", "/tmp/dest"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got, _ := os.ReadFile(os.TempDir() + "/dbarenactl-bench-fetch-args.txt")
	if strings.TrimSpace(string(got)) != "fetch run-1 --dest /tmp/dest" {
		t.Errorf("args = %q", got)
	}

	failing := New(fakeBenchctl(t, `echo "disk full" >&2; exit 1`))
	if err := failing.Fetch(context.Background(), "run-1", "/tmp/dest"); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("err = %v", err)
	}
}

func TestTeardown_BuildsExpectedArgsAndPropagatesFailure(t *testing.T) {
	bin := fakeBenchctl(t, `echo "$@" > `+os.TempDir()+`/dbarenactl-bench-teardown-args.txt`)
	c := New(bin)
	if err := c.Teardown(context.Background(), "run-1"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	got, _ := os.ReadFile(os.TempDir() + "/dbarenactl-bench-teardown-args.txt")
	if strings.TrimSpace(string(got)) != "teardown run-1" {
		t.Errorf("args = %q", got)
	}

	failing := New(fakeBenchctl(t, `echo "AWS creds expired" >&2; exit 1`))
	if err := failing.Teardown(context.Background(), "run-1"); err == nil || !strings.Contains(err.Error(), "AWS creds expired") {
		t.Errorf("err = %v", err)
	}
}

func TestFetchAndTeardown_AlsoWriteToRunLogFile(t *testing.T) {
	bin := fakeBenchctl(t, `echo "$0 called with: $@"`)

	fetching := New(bin)
	fetching.LogDir = t.TempDir()
	if err := fetching.Fetch(context.Background(), "run-1", "/tmp/dest"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(fetching.LogDir, "run-1.log")); err != nil || !strings.Contains(string(got), "fetch run-1 --dest /tmp/dest") {
		t.Errorf("Fetch log = %q, err = %v", got, err)
	}

	tearingDown := New(bin)
	tearingDown.LogDir = t.TempDir()
	if err := tearingDown.Teardown(context.Background(), "run-1"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(tearingDown.LogDir, "run-1.log")); err != nil || !strings.Contains(string(got), "teardown run-1") {
		t.Errorf("Teardown log = %q, err = %v", got, err)
	}
}

func TestNew_DefaultsToBenchctlOnPath(t *testing.T) {
	c := New("")
	if c.BinPath != "benchctl" {
		t.Errorf("BinPath = %q, want %q", c.BinPath, "benchctl")
	}
}

func TestRunState_ProvisionCompleted(t *testing.T) {
	st := &RunState{Phases: map[string]string{"provision": "completed"}}
	if !st.ProvisionCompleted() {
		t.Error("expected ProvisionCompleted true")
	}
	st.Phases["provision"] = "running"
	if st.ProvisionCompleted() {
		t.Error("expected ProvisionCompleted false")
	}
}

func TestRunState_HandedOff(t *testing.T) {
	cases := []struct {
		name   string
		phases map[string]string
		want   bool
	}{
		{"only provision completed", map[string]string{"provision": "completed"}, false},
		{"provision completed, driver.setup pending", map[string]string{"provision": "completed", "driver.setup": "pending"}, false},
		{"driver.setup running", map[string]string{"provision": "completed", "driver.setup": "running"}, true},
		{"driver.setup completed", map[string]string{"provision": "completed", "driver.setup": "completed"}, true},
		{"workload.execute running", map[string]string{"provision": "completed", "driver.setup": "completed", "workload.execute": "running"}, true},
		{"driver.setup failed still counts as handed off", map[string]string{"provision": "completed", "driver.setup": "failed"}, true},
		{"empty phases", map[string]string{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &RunState{Phases: tc.phases}
			if got := st.HandedOff(); got != tc.want {
				t.Errorf("HandedOff() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRunState_Stale(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-1 * time.Minute)
	old := now.Add(-11 * time.Minute)

	cases := []struct {
		name   string
		phases map[string]string
		hb     *time.Time
		want   bool
	}{
		{"not executing", map[string]string{"workload.execute": "completed"}, &old, false},
		{"executing, fresh heartbeat", map[string]string{"workload.execute": "running"}, &fresh, false},
		{"executing, stale heartbeat", map[string]string{"workload.execute": "running"}, &old, true},
		{"executing, no heartbeat yet", map[string]string{"workload.execute": "running"}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &RunState{Phases: tc.phases, LastHeartbeat: tc.hb}
			if got := st.Stale(now); got != tc.want {
				t.Errorf("Stale() = %v, want %v", got, tc.want)
			}
		})
	}
}

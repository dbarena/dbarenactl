package lock

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func TestAcquireRelease(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sweep.lock")

	l, err := Acquire(p)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("lock file not created: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("lock file should be removed after Release, stat err = %v", err)
	}
}

func TestAcquire_FailsWhileHeldByLiveProcess(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sweep.lock")

	l, err := Acquire(p)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer l.Release()

	if _, err := Acquire(p); !errors.Is(err, ErrHeld) {
		t.Errorf("second Acquire err = %v, want ErrHeld", err)
	}
}

func TestAcquire_ReclaimsStaleLockFromDeadProcess(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sweep.lock")

	// A pid that is guaranteed not to be alive: spawn and wait for a
	// short-lived child, then use its now-exited pid.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn short-lived process: %v", err)
	}
	deadPID := cmd.Process.Pid

	if err := os.WriteFile(p, []byte(strconv.Itoa(deadPID)+"\n2020-01-01T00:00:00Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	l, err := Acquire(p)
	if err != nil {
		t.Fatalf("Acquire over stale lock: %v", err)
	}
	defer l.Release()

	content, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := parsePID(string(content)); got != os.Getpid() {
		t.Errorf("lock file pid = %d, want current pid %d", got, os.Getpid())
	}
}

func TestAcquire_CreatesMissingParentDirectory(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nested", "sweeps", "sweep.lock")
	l, err := Acquire(p)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()
	if _, err := os.Stat(p); err != nil {
		t.Errorf("lock file not created: %v", err)
	}
}

func TestAcquire_MissingLockFileSucceeds(t *testing.T) {
	p := filepath.Join(t.TempDir(), "does-not-exist.lock")
	l, err := Acquire(p)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	l.Release()
}

func TestRelease_IdempotentIfAlreadyRemoved(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sweep.lock")
	l, err := Acquire(p)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(p)
	if err := l.Release(); err != nil {
		t.Errorf("Release after external removal: %v", err)
	}
}

func TestParsePID(t *testing.T) {
	cases := []struct {
		content string
		want    int
		ok      bool
	}{
		{"1234\n2026-01-01T00:00:00Z\n", 1234, true},
		{"", 0, false},
		{"not-a-pid\n", 0, false},
		{"-5\n", 0, false},
		{"0\n", 0, false},
	}
	for _, tc := range cases {
		got, ok := parsePID(tc.content)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parsePID(%q) = (%d, %v), want (%d, %v)", tc.content, got, ok, tc.want, tc.ok)
		}
	}
}

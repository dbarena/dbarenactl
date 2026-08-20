// Package lock provides a per-sweep lock file so two dbarenactl processes never
// manage the same sweep concurrently. This is what makes orphan detection in
// internal/scheduler unambiguous: if the lock was free, no other dbarenactl
// process could have had that sweep's in-flight local work alive, so a run
// still sitting in a local-only phase is definitely abandoned, not just slow.
package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrHeld is returned by Acquire when another live process already holds
// the lock.
var ErrHeld = errors.New("lock: already held by another process")

// Lock is a held per-sweep lock. Release removes the underlying file.
type Lock struct {
	path string
}

// Acquire takes the lock at path, which is expected to be a path derived
// from the sweep id (e.g. ~/.dbarenactl/sweeps/<sweep-id>.lock). It fails with
// ErrHeld if the lock file exists and names a PID that is still alive;
// a lock file naming a dead PID (e.g. left behind by a killed process or a
// reboot) is treated as stale and silently reclaimed.
func Acquire(path string) (*Lock, error) {
	if existing, err := os.ReadFile(path); err == nil {
		if pid, ok := parsePID(string(existing)); ok && processAlive(pid) {
			return nil, fmt.Errorf("%w (pid %d, %s)", ErrHeld, pid, path)
		}
		// Stale lock: the recorded process is gone (crash, reboot, or a
		// plain `kill`). Safe to reclaim.
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("lock: read %s: %w", path, err)
	}

	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("lock: mkdir %s: %w", dir, err)
		}
	}
	content := fmt.Sprintf("%d\n%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return nil, fmt.Errorf("lock: write %s: %w", path, err)
	}
	return &Lock{path: path}, nil
}

// Release removes the lock file. Safe to call even if the file was already
// removed by something else.
func (l *Lock) Release() error {
	if err := os.Remove(l.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("lock: remove %s: %w", l.path, err)
	}
	return nil
}

func parsePID(content string) (int, bool) {
	line, _, _ := strings.Cut(content, "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

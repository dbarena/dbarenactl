//go:build !windows

package lock

import (
	"os"
	"syscall"
)

// processAlive reports whether pid names a live process, by sending the
// null signal (no actual signal delivered, just an existence/permission
// check) -- the standard portable-on-Unix way to test liveness.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

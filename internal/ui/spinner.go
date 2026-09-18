// Package ui provides small terminal-feedback primitives shared across
// dbarenactl's subcommands, so long-running operations (pricing fetch today;
// launching/tearing down environments later) look and feel consistent.
package ui

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/mattn/go-isatty"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const spinnerInterval = 100 * time.Millisecond

// Spinner gives feedback for one long-running operation: an animated line
// while it's in progress, frozen into a "✓"/"✗" line once it finishes. In
// non-interactive contexts (piped output, CI) it degrades to plain start/end
// lines instead of carriage-return animation.
type Spinner struct {
	message string
	out     io.Writer
	isTTY   bool

	mu      sync.Mutex
	started bool
	stopped bool
	stop    chan struct{}
	done    chan struct{}
}

// New returns a Spinner that writes to os.Stderr, so it never interleaves
// with a command's actual stdout output (e.g. printSnapshotSummary).
func New(message string) *Spinner {
	return NewWithWriter(os.Stderr, message)
}

// NewWithWriter returns a Spinner that writes to out instead of os.Stderr,
// for callers that inject their own writer (e.g. a component that already
// takes an io.Writer for testability). TTY detection applies to anything
// that can report a file descriptor -- *os.File itself, or a wrapper that
// forwards one, such as the scheduler's serialized output writer. Anything
// else degrades to the same plain start/end lines non-interactive contexts
// already get.
func NewWithWriter(out io.Writer, message string) *Spinner {
	isTTY := false
	if f, ok := out.(interface{ Fd() uintptr }); ok {
		isTTY = isatty.IsTerminal(f.Fd())
	}
	return &Spinner{
		message: message,
		out:     out,
		isTTY:   isTTY,
	}
}

// Start begins showing progress for the operation. Callers must eventually
// call Succeed or Fail exactly once.
func (s *Spinner) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return
	}
	s.started = true

	if !s.isTTY {
		fmt.Fprintf(s.out, "%s...\n", s.message)
		return
	}

	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	go s.animate()
}

func (s *Spinner) animate() {
	defer close(s.done)
	ticker := time.NewTicker(spinnerInterval)
	defer ticker.Stop()
	frame := 0
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			fmt.Fprintf(s.out, "\r\x1b[2K%s %s", spinnerFrames[frame%len(spinnerFrames)], s.message)
			frame++
		}
	}
}

// Succeed stops the spinner and freezes a "✓ message" line.
func (s *Spinner) Succeed(message string) {
	s.finish("✓", message)
}

// Fail stops the spinner and freezes a "✗ message" line.
func (s *Spinner) Fail(message string) {
	s.finish("✗", message)
}

func (s *Spinner) finish(mark, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.stopped = true

	if s.isTTY && s.started {
		close(s.stop)
		<-s.done
		fmt.Fprintf(s.out, "\r\x1b[2K%s %s\n", mark, message)
		return
	}
	fmt.Fprintf(s.out, "%s %s\n", mark, message)
}

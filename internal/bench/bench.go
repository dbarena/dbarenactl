// Package bench wraps the benchctl CLI as a subprocess: launching async
// runs with a caller-supplied run id, polling status, fetching artifacts,
// and tearing down. dbarenactl never talks to benchctl's state store directly
// -- it only ever shells out to the benchctl binary, exactly like a human
// operator would, so behavior never drifts from what `benchctl` itself does.
package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Runner is the subset of benchctl operations the scheduler needs. Defined
// as an interface so the scheduler can be tested against a fake without
// shelling out to a real binary.
type Runner interface {
	// LaunchAsync runs `benchctl run --async --run-id <runID> <scenarioPath>
	// --set k=v ...`. A non-nil error means the launch did not succeed --
	// the caller cannot assume benchctl's state store has any record of
	// runID (it may or may not, depending on how far the attempt got).
	LaunchAsync(ctx context.Context, runID, scenarioPath string, set map[string]string) error
	// Status returns benchctl's current view of runID. Returns ErrRunNotFound
	// if benchctl's store has no record of it at all (e.g. the launch never
	// got far enough to create one).
	Status(ctx context.Context, runID string) (*RunState, error)
	// Fetch runs `benchctl fetch <runID> --dest <localDest>`.
	Fetch(ctx context.Context, runID, localDest string) error
	// Teardown runs `benchctl teardown <runID>`.
	Teardown(ctx context.Context, runID string) error
}

// ErrRunNotFound is returned by Status when benchctl's store has no record
// of the given run id.
var ErrRunNotFound = fmt.Errorf("bench: run not found in benchctl's state store")

// ErrBenchctlUnusable indicates the benchctl binary itself could not be
// invoked (missing, not executable, wrong permissions, etc.), as opposed to
// benchctl running and reporting a domain-level error. This is deterministic
// -- it will fail identically on every retry -- so callers should treat it
// as fatal rather than transient.
var ErrBenchctlUnusable = errors.New("bench: benchctl binary could not be executed")

// RunState mirrors the fields of benchctl's runstate.State that dbarenactl's
// scheduler needs, decoded from `benchctl status <run-id> --output json`.
// dbarenactl can't import benchctl's internal package directly (separate Go
// modules, internal/ visibility), so this is a compatible subset redefined
// here -- keep field names/JSON tags in sync with benchctl's runstate.State.
type RunState struct {
	RunID         string            `json:"run_id"`
	Phases        map[string]string `json:"phases"`
	CompletedAt   *time.Time        `json:"completed_at,omitempty"`
	TerminatedAt  *time.Time        `json:"terminated_at,omitempty"`
	LastHeartbeat *time.Time        `json:"last_heartbeat,omitempty"`
	Error         string            `json:"error,omitempty"`
}

// Phase names, matching benchctl's internal/runstate.Phase constants.
const (
	PhaseProvision       = "provision"
	PhaseDriverSetup     = "driver.setup"
	PhaseWorkloadPrepare = "workload.prepare"
	PhaseWorkloadExecute = "workload.execute"
	PhaseDriverCollect   = "driver.collect"
	PhaseTeardown        = "teardown"
)

// ProvisionCompleted reports whether the target+driver infrastructure
// finished provisioning. This is the only phase `benchctl run --async`
// itself ever writes locally -- see HandedOff.
func (s *RunState) ProvisionCompleted() bool {
	return s.Phases[PhaseProvision] == "completed"
}

// HandedOff reports whether control has passed to the remote driver
// process. Critically, "driver.setup" and every phase after it are written
// *exclusively* by the remote `benchctl resume` process once handoff
// succeeds -- RunAsync itself never touches them (it only ever writes
// "provision", then calls Bootstrap with no phase marker of its own). So
// observing any of them at all is proof the remote process is alive and
// progressing, regardless of what happened to the local machine since.
func (s *RunState) HandedOff() bool {
	for _, phase := range []string{PhaseDriverSetup, PhaseWorkloadPrepare, PhaseWorkloadExecute, PhaseDriverCollect} {
		switch s.Phases[phase] {
		case "running", "completed", "failed":
			return true
		}
	}
	return false
}

// HandoffGracePeriod bounds how long the scheduler waits, after finding a
// run with ProvisionCompleted but not yet HandedOff on resume, before
// concluding the local `benchctl run --async` process died mid-Bootstrap
// (orphaned) rather than the remote resume process simply not having
// reported in yet. Bootstrap's own ssh/scp calls normally take seconds, so
// this is generous headroom against network hiccups, not a guess at
// benchmark duration -- unlike StaleThreshold below, which genuinely can't
// be bounded this tightly.
const HandoffGracePeriod = 5 * time.Minute

// StaleThreshold mirrors benchctl's own staleThreshold for workload.execute
// heartbeats (cmd/benchctl/status.go). Used only to decide when to surface a
// warning -- dbarenactl never auto-tears-down on this signal alone (see the
// design plan: a stale heartbeat can mean a dead benchmark, or a live one
// whose driver-side store token expired).
const StaleThreshold = 10 * time.Minute

// Stale reports whether a still-executing run's heartbeat is old enough to
// warrant a warning.
func (s *RunState) Stale(now time.Time) bool {
	if s.Phases[PhaseWorkloadExecute] != "running" || s.LastHeartbeat == nil {
		return false
	}
	return now.Sub(*s.LastHeartbeat) > StaleThreshold
}

// TerminatedWithoutCompleting reports whether the environment came down
// (TerminatedAt set) before the workload ever reported a result (CompletedAt
// unset) -- e.g. someone ran `benchctl teardown` directly on a run stuck for
// good. Unlike Stale, this is unambiguous: once an environment is torn down,
// nothing further can ever complete under this run id.
func (s *RunState) TerminatedWithoutCompleting() bool {
	return s.CompletedAt == nil && s.TerminatedAt != nil
}

// Client is the real Runner, invoking the benchctl binary as a subprocess.
type Client struct {
	// BinPath is the benchctl executable, resolved via exec.LookPath if not
	// absolute. Defaults to "benchctl".
	BinPath string
	// LogDir, if set, causes every "action" invocation (LaunchAsync, Fetch,
	// Teardown -- not the frequent periodic Status polls) to stream its
	// combined stdout/stderr live into <LogDir>/<runID>.log as the
	// subprocess produces it, so concurrently-running environments never
	// mix their output, a failure's full benchctl output can be inspected
	// without dumping it into the terminal, and `tail -f` can follow a
	// long-running invocation (e.g. bootstrap) while it's still in flight.
	// Empty means logging is disabled.
	LogDir string
}

// New returns a Client using binPath, or "benchctl" (resolved via PATH) if
// binPath is empty.
func New(binPath string) *Client {
	if binPath == "" {
		binPath = "benchctl"
	}
	return &Client{BinPath: binPath}
}

// CheckAvailable verifies binPath can be executed, without actually invoking
// benchctl -- used as a fast preflight check before doing other work (e.g.
// creating a sweep) that would otherwise have to be unwound the moment the
// first real invocation hits the same problem.
func CheckAvailable(binPath string) error {
	if binPath == "" {
		binPath = "benchctl"
	}
	if _, err := exec.LookPath(binPath); err != nil {
		return fmt.Errorf("%w: %v", ErrBenchctlUnusable, err)
	}
	return nil
}

func (c *Client) LaunchAsync(ctx context.Context, runID, scenarioPath string, set map[string]string) error {
	args := []string{"run", "--async", "--run-id", runID, scenarioPath}
	for _, k := range sortedKeys(set) {
		args = append(args, "--set", k+"="+set[k])
	}
	_, stderr, err := c.runLogged(ctx, runID, args...)
	if err != nil {
		return c.runError("launch", runID, err, stderr)
	}
	return nil
}

func (c *Client) Status(ctx context.Context, runID string) (*RunState, error) {
	stdout, stderr, err := c.run(ctx, "status", runID, "--output", "json")
	if err != nil {
		// benchctl's local/remote stores both report a missing run as a
		// plain error, not a distinguishable exit code -- match on the
		// message rather than invent a new benchctl-side contract for this.
		if bytes.Contains(stderr, []byte("no local state found")) || bytes.Contains(stderr, []byte("not found")) {
			return nil, ErrRunNotFound
		}
		return nil, fmt.Errorf("bench: status %s: %w: %s", runID, err, stderr)
	}
	var st RunState
	if err := json.Unmarshal(stdout, &st); err != nil {
		return nil, fmt.Errorf("bench: status %s: parse json: %w", runID, err)
	}
	return &st, nil
}

func (c *Client) Fetch(ctx context.Context, runID, localDest string) error {
	_, stderr, err := c.runLogged(ctx, runID, "fetch", runID, "--dest", localDest)
	if err != nil {
		return c.runError("fetch", runID, err, stderr)
	}
	return nil
}

func (c *Client) Teardown(ctx context.Context, runID string) error {
	_, stderr, err := c.runLogged(ctx, runID, "teardown", runID)
	if err != nil {
		return c.runError("teardown", runID, err, stderr)
	}
	return nil
}

func (c *Client) run(ctx context.Context, args ...string) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, c.BinPath, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = classifyErr(cmd.Run(), ctx)
	return outBuf.Bytes(), errBuf.Bytes(), err
}

// classifyErr distinguishes "the benchctl process never started" (missing
// binary, no execute permission) from "benchctl ran and exited non-zero" --
// the former is deterministic and will fail identically on every retry,
// unlike a real domain-level error from benchctl itself.
func classifyErr(err error, ctx context.Context) error {
	if err != nil && ctx.Err() == nil {
		var execErr *exec.Error
		if errors.As(err, &execErr) || errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrNotExist) {
			err = fmt.Errorf("%w: %v", ErrBenchctlUnusable, err)
		}
	}
	return err
}

// errSuffix returns the text appended after the wrapped error in an action
// method's error message: a pointer to the run's log file if logging is
// configured, otherwise the raw stderr inline (today's behavior, kept for
// callers -- e.g. tests -- that never set LogDir).
func (c *Client) errSuffix(runID string, stderr []byte) string {
	if c.LogDir != "" {
		return fmt.Sprintf(" (see %s for full output)", c.logPath(runID))
	}
	return fmt.Sprintf(": %s", stderr)
}

// RunError is returned by LaunchAsync/Fetch/Teardown when the underlying
// benchctl invocation fails. Its Error() text matches the plain
// "bench: <action> <runID>: <err><suffix>" string but enables printing a short
// summary instead of re-serializing everything.
type RunError struct {
	Action  string
	RunID   string
	Err     error
	LogPath string
	Suffix  string
}

func (e *RunError) Error() string {
	return fmt.Sprintf("bench: %s %s: %v%s", e.Action, e.RunID, e.Err, e.Suffix)
}

func (e *RunError) Unwrap() error { return e.Err }

func (c *Client) runError(action, runID string, err error, stderr []byte) error {
	re := &RunError{Action: action, RunID: runID, Err: err, Suffix: c.errSuffix(runID, stderr)}
	if c.LogDir != "" {
		re.LogPath = c.logPath(runID)
	}
	return re
}

// runLogged behaves like run, but -- when LogDir is configured -- opens
// <LogDir>/<runID>.log up front and wires the subprocess's stdout/stderr
// directly to it, so the file exists and streams live from the moment the
// subprocess starts (rather than being written only after it exits). Used
// only by the "action" methods (LaunchAsync, Fetch, Teardown); Status polls
// too frequently to be worth logging and calls run directly.
func (c *Client) runLogged(ctx context.Context, runID string, args ...string) (stdout, stderr []byte, err error) {
	if c.LogDir == "" {
		return c.run(ctx, args...)
	}

	logFile, openErr := c.openLogFile(runID, args)
	if openErr != nil {
		fmt.Fprintf(os.Stderr, "bench: warning: could not open log file for %s: %v\n", runID, openErr)
		return c.run(ctx, args...)
	}
	defer logFile.Close()

	cmd := exec.CommandContext(ctx, c.BinPath, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	err = classifyErr(cmd.Run(), ctx)
	if err != nil {
		fmt.Fprintf(logFile, "--- error: %v ---\n", err)
	}
	return nil, nil, err
}

// openLogFile creates c.LogDir if needed, opens/creates
// <LogDir>/<runID>.log for appending, and writes a header recording the
// invocation before returning it -- ready to be wired up as the
// subprocess's stdout/stderr directly.
func (c *Client) openLogFile(runID string, args []string) (*os.File, error) {
	if err := os.MkdirAll(c.LogDir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(c.logPath(runID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(f, "=== %s benchctl %s\n", time.Now().UTC().Format(time.RFC3339), strings.Join(args, " "))
	return f, nil
}

// logPath returns where runID's benchctl output log lives.
func (c *Client) logPath(runID string) string {
	return filepath.Join(c.LogDir, runID+".log")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

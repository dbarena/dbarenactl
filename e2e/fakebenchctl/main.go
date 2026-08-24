// fakebenchctl stands in for the real benchctl CLI in dbarenactl's e2e test
// suite (see the e2e package). It implements just enough of benchctl's
// surface for internal/bench.Client to drive:
//
//	fakebenchctl run --async --run-id <id> <scenario> [--set k=v ...]
//	fakebenchctl status <id> --output json
//	fakebenchctl fetch <id> --dest <path>
//	fakebenchctl teardown <id>
//
// Bootstrap (part of `run`) and teardown are blocking, matching benchctl's
// own contract; the workload itself runs in a detached worker process
// spawned by `run`, so dbarenactl's LaunchAsync call returns as soon as the
// environment is ready -- exactly like the real async workflow.
//
// Behavior is entirely config-driven (see Config/Behavior below) rather than
// hardcoded, so the same binary can play every role an e2e scenario needs:
// a benchmark that succeeds, one that fails, one whose environment never
// starts, one whose results can't be fetched, one that can't be torn down.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dbarena/dbarenactl/internal/sweepid"
)

// Behavior configures how one test point's runs behave.
type Behavior struct {
	// Outcome is "success" or "fail" -- how the simulated workload ends.
	// Ignored if LaunchFails is set (the workload never starts).
	Outcome string `json:"outcome"`
	// WorkloadDuration is how long workload.execute stays "running" before
	// resolving to Outcome. Defaults to 300ms if empty.
	WorkloadDuration string `json:"workload_duration"`
	// LaunchFails makes `run --async` itself fail during bootstrap, before
	// any state is recorded -- nothing was ever provisioned.
	LaunchFails bool `json:"launch_fails"`
	// FetchFails makes `fetch` fail every time, regardless of Outcome.
	FetchFails bool `json:"fetch_fails"`
	// TeardownFails makes `teardown` fail every time.
	TeardownFails bool `json:"teardown_fails"`
}

// Config is loaded from the file named by FAKEBENCH_CONFIG. TestPoints is
// keyed the same way manifest.TestPointDef.Key() formats a test point:
// "<tier>/<bound_type>" or "<tier>/<bound_type>/<variant>".
type Config struct {
	BootstrapDuration string              `json:"bootstrap_duration"`
	TeardownDuration  string              `json:"teardown_duration"`
	Default           Behavior            `json:"default"`
	TestPoints        map[string]Behavior `json:"test_points"`
}

type state struct {
	RunID         string            `json:"run_id"`
	Phases        map[string]string `json:"phases"`
	CompletedAt   *time.Time        `json:"completed_at,omitempty"`
	TerminatedAt  *time.Time        `json:"terminated_at,omitempty"`
	LastHeartbeat *time.Time        `json:"last_heartbeat,omitempty"`
	Error         string            `json:"error,omitempty"`
}

func stateDir() string {
	d := os.Getenv("FAKEBENCH_STATE_DIR")
	if d == "" {
		fatal("FAKEBENCH_STATE_DIR is not set")
	}
	return d
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fakebenchctl: "+format+"\n", args...)
	os.Exit(1)
}

func loadConfig() *Config {
	path := os.Getenv("FAKEBENCH_CONFIG")
	if path == "" {
		fatal("FAKEBENCH_CONFIG is not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		fatal("read config: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		fatal("parse config: %v", err)
	}
	return &cfg
}

// testPointSlug rebuilds the substring a run id derived from key
// ("tier/bound_type[/variant]") would contain, mirroring how
// internal/planner.TestPointID assembles a test point's id from a sweep id
// plus its slugified tier/bound_type/variant.
func testPointSlug(key string) string {
	parts := strings.Split(key, "/")
	slugs := make([]string, 0, len(parts))
	for _, p := range parts {
		slugs = append(slugs, sweepid.Slugify(p))
	}
	return strings.Join(slugs, "-")
}

// behaviorFor matches runID against every configured test point's slug,
// preferring the longest (most specific) match, and falls back to
// cfg.Default if nothing matches.
func behaviorFor(cfg *Config, runID string) Behavior {
	type candidate struct {
		slug string
		b    Behavior
	}
	var candidates []candidate
	for key, b := range cfg.TestPoints {
		candidates = append(candidates, candidate{testPointSlug(key), b})
	}
	sort.Slice(candidates, func(i, j int) bool { return len(candidates[i].slug) > len(candidates[j].slug) })
	for _, c := range candidates {
		if strings.Contains(runID, c.slug) {
			return c.b
		}
	}
	return cfg.Default
}

func durationOr(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		fatal("invalid duration %q: %v", s, err)
	}
	return d
}

func statePath(dir, runID string) string { return filepath.Join(dir, runID+".json") }
func pidPath(dir, runID string) string   { return filepath.Join(dir, runID+".pid") }

func loadState(dir, runID string) (*state, error) {
	data, err := os.ReadFile(statePath(dir, runID))
	if err != nil {
		return nil, err
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func saveState(dir string, st *state) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := statePath(dir, st.RunID) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(dir, st.RunID))
}

func logf(dir, runID, format string, args ...any) {
	f, err := os.OpenFile(filepath.Join(dir, runID+".worker.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] "+format+"\n", append([]any{time.Now().UTC().Format(time.RFC3339)}, args...)...)
}

func main() {
	if len(os.Args) < 2 {
		fatal("missing subcommand")
	}
	switch os.Args[1] {
	case "run":
		cmdRun(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "fetch":
		cmdFetch(os.Args[2:])
	case "teardown":
		cmdTeardown(os.Args[2:])
	case "__worker":
		cmdWorker(os.Args[2:])
	default:
		fatal("unknown subcommand %q", os.Args[1])
	}
}

// cmdRun implements `run --async --run-id <id> <scenario> [--set k=v ...]`.
// Bootstrap is blocking and happens here; the workload itself is handed off
// to a detached worker so this process can return as soon as the
// environment is ready, without waiting for the workload to finish.
func cmdRun(args []string) {
	dir := stateDir()
	cfg := loadConfig()

	var runID string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--async":
		case "--run-id":
			i++
			runID = args[i]
		case "--set":
			i++
		}
	}
	if runID == "" {
		fatal("run: missing --run-id")
	}

	b := behaviorFor(cfg, runID)
	bootstrap := durationOr(cfg.BootstrapDuration, 200*time.Millisecond)
	time.Sleep(bootstrap)

	if b.LaunchFails {
		// Nothing was ever provisioned -- no state file, nothing to tear
		// down.
		fatal("run %s: simulated launch failure", runID)
	}

	st := &state{RunID: runID, Phases: map[string]string{"provision": "completed"}}
	if err := saveState(dir, st); err != nil {
		fatal("run: %v", err)
	}

	self, err := os.Executable()
	if err != nil {
		fatal("run: %v", err)
	}
	logFile, err := os.OpenFile(filepath.Join(dir, runID+".worker.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fatal("run: %v", err)
	}
	defer logFile.Close()

	worker := exec.Command(self, "__worker", "--run-id", runID)
	worker.Env = os.Environ()
	worker.Stdout = logFile
	worker.Stderr = logFile
	worker.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := worker.Start(); err != nil {
		fatal("run: start worker: %v", err)
	}
	_ = os.WriteFile(pidPath(dir, runID), []byte(strconv.Itoa(worker.Process.Pid)), 0o644)
	_ = worker.Process.Release()

	fmt.Printf("launched %s\n", runID)
}

// cmdWorker is the detached async worker spawned by cmdRun. Never invoked
// directly by dbarenactl.
func cmdWorker(args []string) {
	dir := stateDir()
	cfg := loadConfig()

	var runID string
	for i := 0; i < len(args); i++ {
		if args[i] == "--run-id" {
			i++
			runID = args[i]
		}
	}
	st, err := loadState(dir, runID)
	if err != nil {
		logf(dir, runID, "worker: load state: %v", err)
		return
	}
	b := behaviorFor(cfg, runID)

	setPhase := func(phase, status string) {
		st.Phases[phase] = status
		if err := saveState(dir, st); err != nil {
			logf(dir, runID, "worker: save state: %v", err)
		}
	}
	heartbeat := func() {
		now := time.Now().UTC()
		st.LastHeartbeat = &now
		if err := saveState(dir, st); err != nil {
			logf(dir, runID, "worker: save state: %v", err)
		}
	}
	complete := func(errMsg string) {
		now := time.Now().UTC()
		st.CompletedAt = &now
		st.Error = errMsg
		if err := saveState(dir, st); err != nil {
			logf(dir, runID, "worker: save state: %v", err)
		}
	}

	setPhase("driver.setup", "running")
	time.Sleep(100 * time.Millisecond)
	setPhase("driver.setup", "completed")

	setPhase("workload.prepare", "running")
	time.Sleep(100 * time.Millisecond)
	setPhase("workload.prepare", "completed")

	setPhase("workload.execute", "running")
	heartbeat()
	time.Sleep(durationOr(b.WorkloadDuration, 300*time.Millisecond))

	if b.Outcome == "fail" {
		setPhase("workload.execute", "failed")
		setPhase("driver.collect", "completed")
		complete("simulated persistent workload failure: workload exited with status 1")
	} else {
		setPhase("workload.execute", "completed")
		setPhase("driver.collect", "completed")
		complete("")
	}

	// The environment may still be up (fetch/teardown haven't run yet), but
	// this process's job -- running the workload -- is done; nothing left
	// to clean up for *this* pid.
	_ = os.Remove(pidPath(dir, runID))
	logf(dir, runID, "worker: done, error=%q", st.Error)
}

// cmdStatus implements `status <id> --output json`.
func cmdStatus(args []string) {
	if len(args) == 0 {
		fatal("status: missing run id")
	}
	dir := stateDir()
	runID := args[0]
	st, err := loadState(dir, runID)
	if err != nil {
		fatal("status: no local state found for run %s", runID)
	}
	data, err := json.Marshal(st)
	if err != nil {
		fatal("status: %v", err)
	}
	os.Stdout.Write(data)
	fmt.Println()
}

// cmdFetch implements `fetch <id> --dest <path>`.
func cmdFetch(args []string) {
	if len(args) < 3 || args[1] != "--dest" {
		fatal("fetch: usage: fetch <run-id> --dest <path>")
	}
	dir := stateDir()
	cfg := loadConfig()
	runID, dest := args[0], args[2]

	st, err := loadState(dir, runID)
	if err != nil {
		fatal("fetch: no local state found for run %s", runID)
	}
	if behaviorFor(cfg, runID).FetchFails {
		fatal("fetch %s: simulated fetch failure", runID)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		fatal("fetch: %v", err)
	}
	outcome := "success"
	if st.Error != "" {
		outcome = "failure"
	}
	summary := fmt.Sprintf(`{"run_id":%q,"outcome":%q,"error":%q}`+"\n", runID, outcome, st.Error)
	if err := os.WriteFile(filepath.Join(dest, "result.json"), []byte(summary), 0o644); err != nil {
		fatal("fetch: %v", err)
	}
	fmt.Printf("fetched %s to %s\n", runID, dest)
}

// cmdTeardown implements `teardown <id>`. Blocking, like bootstrap.
func cmdTeardown(args []string) {
	if len(args) == 0 {
		fatal("teardown: missing run id")
	}
	dir := stateDir()
	cfg := loadConfig()
	runID := args[0]

	time.Sleep(durationOr(cfg.TeardownDuration, 200*time.Millisecond))

	if behaviorFor(cfg, runID).TeardownFails {
		fatal("teardown %s: simulated teardown failure", runID)
	}

	st, err := loadState(dir, runID)
	if err != nil {
		st = &state{RunID: runID, Phases: map[string]string{}}
	}
	st.Phases["teardown"] = "completed"
	now := time.Now().UTC()
	st.TerminatedAt = &now
	if err := saveState(dir, st); err != nil {
		fatal("teardown: %v", err)
	}
	_ = os.Remove(pidPath(dir, runID))
	fmt.Printf("torn down %s\n", runID)
}

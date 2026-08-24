// Package sweepstate is dbarenactl's local, durable record of sweep progress:
// which test points a sweep needs, which benchctl runs exist for each, and
// exactly what to retry after a crash or an intentional stop. It is backed
// by a single SQLite file (see db.go) so every state transition is one
// transaction -- durability and atomicity come from SQLite itself rather
// than bespoke atomic-file-rewrite code, since surviving a crash mid-write
// is the entire point of this package.
package sweepstate

import "time"

// SweepStatus is the coarse lifecycle state of a sweep.
type SweepStatus string

const (
	SweepRunning      SweepStatus = "running"
	SweepStoppedError SweepStatus = "stopped_error"
	SweepCompleted    SweepStatus = "completed"
)

// Sweep is one `dbarenactl run` invocation's worth of work: one provider, one
// full parameter set (see internal/sweepid), and the test points it implies.
type Sweep struct {
	ID         string
	Provider   string
	Workload   string
	ParamsJSON string
	Status     SweepStatus
	// ErrorAction/ErrorTarget/ErrorDetail/ErrorAt are set together when a
	// launch/teardown/fetch operation fails and the sweep stops-the-world.
	// ErrorTarget is the id of the TestPoint (for a launch failure) or Run
	// (for a fetch/teardown failure) to retry first on resume, before any
	// other scheduling happens.
	ErrorAction string
	ErrorTarget string
	ErrorDetail string
	ErrorAt     *time.Time
	CreatedAt   time.Time
}

// HasError reports whether the sweep is currently stopped on an
// infra-operation failure awaiting resume.
func (s *Sweep) HasError() bool { return s.ErrorAction != "" }

// TestPoint is one (tier, bound_type, variant) combination within a sweep.
// SuccessesCount and FailuresCount are independent counters (see
// internal/sweepid doc and the dbarenactl design plan): a test point keeps
// retrying until it accumulates SuccessesNeeded successes, and only gives up
// if FailuresCount reaches FailureBudget first.
type TestPoint struct {
	ID              string
	SweepID         string
	Tier            string
	Workload        string
	Scenario        string
	BoundType       string
	Variant         string
	Set             map[string]string
	SuccessesNeeded int
	SuccessesCount  int
	FailuresCount   int
	FailureBudget   int
}

// Satisfied reports whether this test point has reached its required
// number of successful iterations.
func (tp *TestPoint) Satisfied() bool { return tp.SuccessesCount >= tp.SuccessesNeeded }

// BudgetExhausted reports whether this test point has used up its failure
// budget without reaching Satisfied -- a terminal give-up state that
// requires manual intervention (adjust the manifest/budget and start a new
// sweep), not something a plain `dbarenactl resume` can fix on its own.
func (tp *TestPoint) BudgetExhausted() bool {
	return !tp.Satisfied() && tp.FailuresCount >= tp.FailureBudget
}

// NextAttempt returns the 1-based iteration attempt number for the next run
// to be launched against this test point.
func (tp *TestPoint) NextAttempt() int { return tp.SuccessesCount + tp.FailuresCount + 1 }

// RunStatus tracks a single benchctl run through dbarenactl's side of its
// lifecycle. Everything up to and including RunWaitingRemote can be
// re-derived from benchctl's own state store; RunNeedsResultsPull onward is
// dbarenactl-only bookkeeping for work benchctl has no concept of.
type RunStatus string

const (
	// RunLaunching means dbarenactl has durably recorded intent to launch this
	// run but hasn't yet learned whether `benchctl run --async --run-id`
	// succeeded. Only ever seen mid-flight; if found at the start of a
	// `dbarenactl resume`, it is definitionally orphaned (see the design plan's
	// orphan-detection section) because provisioning is local-only and
	// synchronous, and the per-sweep lock guarantees no other dbarenactl
	// process could still be running it.
	RunLaunching RunStatus = "launching"
	// RunWaitingRemote means the launch handed off successfully; the
	// workload is executing autonomously on the remote driver.
	RunWaitingRemote RunStatus = "waiting_remote"
	// RunNeedsResultsPull means benchctl reports the run finished (success
	// or failure) and its artifacts still need to be fetched locally.
	RunNeedsResultsPull RunStatus = "needs_results_pull"
	// RunNeedsTeardown means artifacts are safely local; the environment
	// still needs to be torn down.
	RunNeedsTeardown RunStatus = "needs_teardown"
	// RunDone and RunFailed are terminal: fully finished (artifacts pulled,
	// environment torn down) with a successful or failed workload outcome
	// respectively.
	RunDone   RunStatus = "done"
	RunFailed RunStatus = "failed"
)

// Terminal reports whether no further action is needed for this run.
func (s RunStatus) Terminal() bool { return s == RunDone || s == RunFailed }

// Run is one benchctl run (one iteration attempt) for a TestPoint.
type Run struct {
	RunID            string
	TestPointID      string
	IterationAttempt int
	Status           RunStatus
	// Outcome is "" until the run reaches RunNeedsResultsPull or later, then
	// "success" or "failure" -- cached from benchctl's own CompletedAt/Error
	// fields so finalizing a run doesn't need to re-query benchctl.
	Outcome          string
	LocalArtifactDir string
	// FetchAttempts counts bounded local retries of a failed results-pull
	// before escalating to the sweep-wide stop-the-world rule.
	FetchAttempts int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

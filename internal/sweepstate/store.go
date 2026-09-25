package sweepstate

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned by Get* methods when no row matches.
var ErrNotFound = errors.New("sweepstate: not found")

// CreateSweep inserts a new sweep row with status running.
func (s *Store) CreateSweep(sw *Sweep) error {
	_, err := s.db.Exec(
		`INSERT INTO sweeps (id, provider, product, plan, workload, params_json, status, created_at, last_started_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sw.ID, sw.Provider, sw.Product, sw.Plan, sw.Workload, sw.ParamsJSON, string(SweepRunning), sw.CreatedAt.UTC(), sw.CreatedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("sweepstate: create sweep %s: %w", sw.ID, err)
	}
	sw.Status = SweepRunning
	sw.LastStartedAt = sw.CreatedAt
	return nil
}

// GetSweep loads a sweep by id.
func (s *Store) GetSweep(id string) (*Sweep, error) {
	row := s.db.QueryRow(
		`SELECT id, provider, product, plan, workload, params_json, status, error_action, error_target, error_detail, error_at, created_at, last_started_at
		 FROM sweeps WHERE id = ?`, id,
	)
	return scanSweep(row)
}

// ListIncompleteSweeps returns every sweep not yet marked completed, most
// recently started (initial run or latest resume) first.
func (s *Store) ListIncompleteSweeps() ([]*Sweep, error) {
	rows, err := s.db.Query(
		`SELECT id, provider, product, plan, workload, params_json, status, error_action, error_target, error_detail, error_at, created_at, last_started_at
		 FROM sweeps WHERE status != ? ORDER BY last_started_at DESC`, string(SweepCompleted),
	)
	if err != nil {
		return nil, fmt.Errorf("sweepstate: list incomplete sweeps: %w", err)
	}
	defer rows.Close()

	var out []*Sweep
	for rows.Next() {
		sw, err := scanSweepRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sw)
	}
	return out, rows.Err()
}

// ListAllSweeps returns every sweep regardless of status, most recently
// started (initial run or latest resume) first.
func (s *Store) ListAllSweeps() ([]*Sweep, error) {
	rows, err := s.db.Query(
		`SELECT id, provider, product, plan, workload, params_json, status, error_action, error_target, error_detail, error_at, created_at, last_started_at
		 FROM sweeps ORDER BY last_started_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("sweepstate: list all sweeps: %w", err)
	}
	defer rows.Close()

	var out []*Sweep
	for rows.Next() {
		sw, err := scanSweepRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sw)
	}
	return out, rows.Err()
}

// TouchLastStarted records that the sweep is being (re)started right now --
// called on every `dbarenactl run`/`resume` invocation that actually
// executes the sweep, so LastStartedAt always reflects the initial run or
// the most recent resume, whichever is later.
func (s *Store) TouchLastStarted(sweepID string, at time.Time) error {
	res, err := s.db.Exec(`UPDATE sweeps SET last_started_at = ? WHERE id = ?`, at.UTC(), sweepID)
	if err != nil {
		return fmt.Errorf("sweepstate: touch last started for sweep %s: %w", sweepID, err)
	}
	return checkOneRowAffected(res, "sweep", sweepID)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSweep(row *sql.Row) (*Sweep, error) {
	sw, err := scanSweepGeneric(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return sw, err
}

func scanSweepRows(rows *sql.Rows) (*Sweep, error) { return scanSweepGeneric(rows) }

func scanSweepGeneric(r rowScanner) (*Sweep, error) {
	var sw Sweep
	var status string
	var errorAt sql.NullTime
	var lastStartedAt sql.NullTime
	if err := r.Scan(&sw.ID, &sw.Provider, &sw.Product, &sw.Plan, &sw.Workload, &sw.ParamsJSON, &status,
		&sw.ErrorAction, &sw.ErrorTarget, &sw.ErrorDetail, &errorAt, &sw.CreatedAt, &lastStartedAt); err != nil {
		return nil, fmt.Errorf("sweepstate: scan sweep: %w", err)
	}
	sw.Status = SweepStatus(status)
	if errorAt.Valid {
		sw.ErrorAt = &errorAt.Time
	}
	if lastStartedAt.Valid {
		sw.LastStartedAt = lastStartedAt.Time
	} else {
		sw.LastStartedAt = sw.CreatedAt
	}
	return &sw, nil
}

// RecordError stops the sweep on an infra-operation failure: sets status to
// stopped_error and records exactly what to retry first on resume.
func (s *Store) RecordError(sweepID, action, target, detail string) error {
	_, err := s.db.Exec(
		`UPDATE sweeps SET status = ?, error_action = ?, error_target = ?, error_detail = ?, error_at = ? WHERE id = ?`,
		string(SweepStoppedError), action, target, detail, time.Now().UTC(), sweepID,
	)
	if err != nil {
		return fmt.Errorf("sweepstate: record error on sweep %s: %w", sweepID, err)
	}
	return nil
}

// ClearError un-stops a sweep after its recorded failed action has been
// retried successfully.
func (s *Store) ClearError(sweepID string) error {
	_, err := s.db.Exec(
		`UPDATE sweeps SET status = ?, error_action = '', error_target = '', error_detail = '', error_at = NULL WHERE id = ?`,
		string(SweepRunning), sweepID,
	)
	if err != nil {
		return fmt.Errorf("sweepstate: clear error on sweep %s: %w", sweepID, err)
	}
	return nil
}

// MarkCompleted marks every test point in the sweep as satisfied.
func (s *Store) MarkCompleted(sweepID string) error {
	_, err := s.db.Exec(`UPDATE sweeps SET status = ? WHERE id = ?`, string(SweepCompleted), sweepID)
	if err != nil {
		return fmt.Errorf("sweepstate: mark sweep %s completed: %w", sweepID, err)
	}
	return nil
}

// ResetSweep discards a completed sweep's test points and runs, and the
// sweep row itself, so the caller can start over from scratch under the
// same id via CreateSweep/CreateTestPoints -- e.g. to gather fresh data for
// a newly deployed version without changing the manifest/flags that define
// the sweep's identity. Only valid on a completed sweep: an incomplete one
// may still have real infra to tear down first, which this does not do.
func (s *Store) ResetSweep(sweepID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("sweepstate: begin reset sweep %s: %w", sweepID, err)
	}
	defer tx.Rollback() //nolint:errcheck

	var status string
	if err := tx.QueryRow(`SELECT status FROM sweeps WHERE id = ?`, sweepID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("sweepstate: reset sweep %s: %w", sweepID, err)
	}
	if status != string(SweepCompleted) {
		return fmt.Errorf("sweepstate: reset sweep %s: not completed (status=%s)", sweepID, status)
	}

	if _, err := tx.Exec(`DELETE FROM runs WHERE test_point_id IN (SELECT id FROM test_points WHERE sweep_id = ?)`, sweepID); err != nil {
		return fmt.Errorf("sweepstate: reset sweep %s: delete runs: %w", sweepID, err)
	}
	if _, err := tx.Exec(`DELETE FROM test_points WHERE sweep_id = ?`, sweepID); err != nil {
		return fmt.Errorf("sweepstate: reset sweep %s: delete test points: %w", sweepID, err)
	}
	if _, err := tx.Exec(`DELETE FROM sweeps WHERE id = ?`, sweepID); err != nil {
		return fmt.Errorf("sweepstate: reset sweep %s: delete sweep: %w", sweepID, err)
	}
	return tx.Commit()
}

// DeleteSweep permanently removes a sweep and all its test points and runs
// from the database, regardless of its current status. Callers must tear
// down any live infra for its non-terminal runs first (see
// ListNonTerminalRuns) -- like ResetSweep/RestartSweep, this is a pure DB
// operation with no knowledge of live infra.
func (s *Store) DeleteSweep(sweepID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("sweepstate: begin delete sweep %s: %w", sweepID, err)
	}
	defer tx.Rollback() //nolint:errcheck

	var status string
	if err := tx.QueryRow(`SELECT status FROM sweeps WHERE id = ?`, sweepID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("sweepstate: delete sweep %s: %w", sweepID, err)
	}

	if _, err := tx.Exec(`DELETE FROM runs WHERE test_point_id IN (SELECT id FROM test_points WHERE sweep_id = ?)`, sweepID); err != nil {
		return fmt.Errorf("sweepstate: delete sweep %s: delete runs: %w", sweepID, err)
	}
	if _, err := tx.Exec(`DELETE FROM test_points WHERE sweep_id = ?`, sweepID); err != nil {
		return fmt.Errorf("sweepstate: delete sweep %s: delete test points: %w", sweepID, err)
	}
	if _, err := tx.Exec(`DELETE FROM sweeps WHERE id = ?`, sweepID); err != nil {
		return fmt.Errorf("sweepstate: delete sweep %s: delete sweep: %w", sweepID, err)
	}
	return tx.Commit()
}

// ExtendExhaustedTestPoints gives every currently-exhausted test point in
// the sweep (successes still short of target, failures at or past budget --
// there can be more than one, since Step only ever records the last one it
// finds in a pass) a fresh failure allowance to keep trying for whatever
// successes it still needs, and un-stops the sweep. Existing successes are
// untouched, so a test point at 2/5 keeps its 2 and just gets more attempts
// at the remaining 3.
func (s *Store) ExtendExhaustedTestPoints(sweepID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("sweepstate: begin extend exhausted test points for sweep %s: %w", sweepID, err)
	}
	defer tx.Rollback() //nolint:errcheck

	var status string
	if err := tx.QueryRow(`SELECT status FROM sweeps WHERE id = ?`, sweepID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("sweepstate: extend exhausted test points for sweep %s: %w", sweepID, err)
	}
	if status != string(SweepStoppedError) {
		return fmt.Errorf("sweepstate: extend exhausted test points for sweep %s: not stopped (status=%s)", sweepID, status)
	}

	if _, err := tx.Exec(
		`UPDATE test_points SET failures_count = 0
		 WHERE sweep_id = ? AND successes_count < successes_needed AND failures_count >= failure_budget`,
		sweepID,
	); err != nil {
		return fmt.Errorf("sweepstate: extend exhausted test points for sweep %s: %w", sweepID, err)
	}
	if _, err := tx.Exec(
		`UPDATE sweeps SET status = ?, error_action = '', error_target = '', error_detail = '', error_at = NULL WHERE id = ?`,
		string(SweepRunning), sweepID,
	); err != nil {
		return fmt.Errorf("sweepstate: extend exhausted test points for sweep %s: clear error: %w", sweepID, err)
	}
	return tx.Commit()
}

// SkipTestPoint permanently excludes one specific test point from the sweep
// going forward -- the scheduler will neither retry it nor let it block the
// sweep from completing -- and un-stops the sweep, exactly like
// ExtendExhaustedTestPoints does, except targeted at a single test point
// rather than every currently-exhausted one. Existing successes/failures on
// the test point are left untouched, only its Skipped flag is set. A later
// RestartSweep clears this flag along with everything else, since "start
// fresh" means redoing the whole sweep, including this test point.
func (s *Store) SkipTestPoint(sweepID, testPointID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("sweepstate: begin skip test point %s for sweep %s: %w", testPointID, sweepID, err)
	}
	defer tx.Rollback() //nolint:errcheck

	var status string
	if err := tx.QueryRow(`SELECT status FROM sweeps WHERE id = ?`, sweepID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("sweepstate: skip test point %s for sweep %s: %w", testPointID, sweepID, err)
	}
	if status != string(SweepStoppedError) {
		return fmt.Errorf("sweepstate: skip test point %s for sweep %s: not stopped (status=%s)", testPointID, sweepID, status)
	}

	if res, err := tx.Exec(`UPDATE test_points SET skipped = 1 WHERE id = ? AND sweep_id = ?`, testPointID, sweepID); err != nil {
		return fmt.Errorf("sweepstate: skip test point %s for sweep %s: %w", testPointID, sweepID, err)
	} else if err := checkOneRowAffected(res, "test point", testPointID); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE sweeps SET status = ?, error_action = '', error_target = '', error_detail = '', error_at = NULL WHERE id = ?`,
		string(SweepRunning), sweepID,
	); err != nil {
		return fmt.Errorf("sweepstate: skip test point %s for sweep %s: clear error: %w", testPointID, sweepID, err)
	}
	return tx.Commit()
}

// UpdateParamsJSON overwrites a sweep's stored parameter blob. Used only to
// persist a runtime override -- e.g. `dbarenactl resume --max-concurrency`
// -- that the CLI deliberately treats as mutable, unlike the rest of a
// sweep's identity (see internal/sweepid).
func (s *Store) UpdateParamsJSON(sweepID, paramsJSON string) error {
	res, err := s.db.Exec(`UPDATE sweeps SET params_json = ? WHERE id = ?`, paramsJSON, sweepID)
	if err != nil {
		return fmt.Errorf("sweepstate: update params for sweep %s: %w", sweepID, err)
	}
	return checkOneRowAffected(res, "sweep", sweepID)
}

// RestartSweep discards every test point's progress in the sweep -- not
// just the one that exhausted its budget -- and un-stops it, so it redoes
// everything from scratch under the same id. Definitional test point
// fields (tier/bound_type/scenario/set/successes_needed/failure_budget)
// are left as-is; only successes_count/failures_count reset to 0 and every
// run row is deleted. Callers must ensure every non-terminal run has
// already been torn down first (see scheduler.RestartExhaustedSweep) --
// this is a pure DB operation with no knowledge of live infra.
func (s *Store) RestartSweep(sweepID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("sweepstate: begin restart sweep %s: %w", sweepID, err)
	}
	defer tx.Rollback() //nolint:errcheck

	var status string
	if err := tx.QueryRow(`SELECT status FROM sweeps WHERE id = ?`, sweepID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("sweepstate: restart sweep %s: %w", sweepID, err)
	}
	if status != string(SweepStoppedError) {
		return fmt.Errorf("sweepstate: restart sweep %s: not stopped (status=%s)", sweepID, status)
	}

	if _, err := tx.Exec(`DELETE FROM runs WHERE test_point_id IN (SELECT id FROM test_points WHERE sweep_id = ?)`, sweepID); err != nil {
		return fmt.Errorf("sweepstate: restart sweep %s: delete runs: %w", sweepID, err)
	}
	if _, err := tx.Exec(`UPDATE test_points SET successes_count = 0, failures_count = 0, skipped = 0 WHERE sweep_id = ?`, sweepID); err != nil {
		return fmt.Errorf("sweepstate: restart sweep %s: reset test points: %w", sweepID, err)
	}
	if _, err := tx.Exec(
		`UPDATE sweeps SET status = ?, error_action = '', error_target = '', error_detail = '', error_at = NULL WHERE id = ?`,
		string(SweepRunning), sweepID,
	); err != nil {
		return fmt.Errorf("sweepstate: restart sweep %s: clear error: %w", sweepID, err)
	}
	return tx.Commit()
}

// CreateTestPoints inserts every test point for a freshly created sweep in
// one transaction.
func (s *Store) CreateTestPoints(tps []*TestPoint) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("sweepstate: begin create test points: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.Prepare(
		`INSERT INTO test_points
		 (id, sweep_id, tier, workload, scenario, bound_type, variant, set_json, successes_needed, failure_budget)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return fmt.Errorf("sweepstate: prepare create test points: %w", err)
	}
	defer stmt.Close()

	for _, tp := range tps {
		setJSON, err := json.Marshal(tp.Set)
		if err != nil {
			return fmt.Errorf("sweepstate: marshal set for %s: %w", tp.ID, err)
		}
		if _, err := stmt.Exec(tp.ID, tp.SweepID, tp.Tier, tp.Workload, tp.Scenario, tp.BoundType, tp.Variant,
			string(setJSON), tp.SuccessesNeeded, tp.FailureBudget); err != nil {
			return fmt.Errorf("sweepstate: insert test point %s: %w", tp.ID, err)
		}
	}
	return tx.Commit()
}

// ListTestPoints returns every test point for a sweep.
func (s *Store) ListTestPoints(sweepID string) ([]*TestPoint, error) {
	rows, err := s.db.Query(
		`SELECT id, sweep_id, tier, workload, scenario, bound_type, variant, set_json,
		        successes_needed, successes_count, failures_count, failure_budget, skipped
		 FROM test_points WHERE sweep_id = ? ORDER BY id`, sweepID,
	)
	if err != nil {
		return nil, fmt.Errorf("sweepstate: list test points for %s: %w", sweepID, err)
	}
	defer rows.Close()

	var out []*TestPoint
	for rows.Next() {
		tp, err := scanTestPoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tp)
	}
	return out, rows.Err()
}

// GetTestPoint loads a single test point by id.
func (s *Store) GetTestPoint(id string) (*TestPoint, error) {
	row := s.db.QueryRow(
		`SELECT id, sweep_id, tier, workload, scenario, bound_type, variant, set_json,
		        successes_needed, successes_count, failures_count, failure_budget, skipped
		 FROM test_points WHERE id = ?`, id,
	)
	tp, err := scanTestPoint(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return tp, err
}

func scanTestPoint(r rowScanner) (*TestPoint, error) {
	var tp TestPoint
	var setJSON string
	var skipped int
	if err := r.Scan(&tp.ID, &tp.SweepID, &tp.Tier, &tp.Workload, &tp.Scenario, &tp.BoundType, &tp.Variant, &setJSON,
		&tp.SuccessesNeeded, &tp.SuccessesCount, &tp.FailuresCount, &tp.FailureBudget, &skipped); err != nil {
		return nil, fmt.Errorf("sweepstate: scan test point: %w", err)
	}
	tp.Skipped = skipped != 0
	if err := json.Unmarshal([]byte(setJSON), &tp.Set); err != nil {
		return nil, fmt.Errorf("sweepstate: unmarshal set for %s: %w", tp.ID, err)
	}
	return &tp, nil
}

// CreateRun inserts a new run row with status launching, recording intent to
// launch it. Callers must call this and have it durably committed *before*
// invoking benchctl, so a crash before benchctl's own state exists is
// unambiguously safe to discard on resume.
func (s *Store) CreateRun(run *Run) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(
		`INSERT INTO runs (run_id, test_point_id, iteration_attempt, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		run.RunID, run.TestPointID, run.IterationAttempt, string(RunLaunching), now, now,
	)
	if err != nil {
		return fmt.Errorf("sweepstate: create run %s: %w", run.RunID, err)
	}
	run.Status = RunLaunching
	run.CreatedAt, run.UpdatedAt = now, now
	return nil
}

// DeleteRun removes a run row entirely. Used only to discard a RunLaunching
// row that turned out to be orphaned (see the scheduler's classifyLaunching):
// such a row never represented a real, counted attempt, so it's deleted
// rather than finalized as a failure -- finalizing it would incorrectly
// consume part of its test point's failure budget for something that never
// actually ran.
func (s *Store) DeleteRun(runID string) error {
	res, err := s.db.Exec(`DELETE FROM runs WHERE run_id = ?`, runID)
	if err != nil {
		return fmt.Errorf("sweepstate: delete run %s: %w", runID, err)
	}
	return checkOneRowAffected(res, "run", runID)
}

// GetRun loads a single run by id.
func (s *Store) GetRun(runID string) (*Run, error) {
	row := s.db.QueryRow(
		`SELECT run_id, test_point_id, iteration_attempt, status, outcome, local_artifact_dir, fetch_attempts, created_at, updated_at
		 FROM runs WHERE run_id = ?`, runID,
	)
	run, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return run, err
}

// ListNonTerminalRuns returns every run for the sweep whose status is not
// yet Terminal(), across all of the sweep's test points.
func (s *Store) ListNonTerminalRuns(sweepID string) ([]*Run, error) {
	rows, err := s.db.Query(
		`SELECT r.run_id, r.test_point_id, r.iteration_attempt, r.status, r.outcome, r.local_artifact_dir, r.fetch_attempts, r.created_at, r.updated_at
		 FROM runs r JOIN test_points tp ON tp.id = r.test_point_id
		 WHERE tp.sweep_id = ? AND r.status NOT IN (?, ?)
		 ORDER BY r.created_at`,
		sweepID, string(RunDone), string(RunFailed),
	)
	if err != nil {
		return nil, fmt.Errorf("sweepstate: list non-terminal runs for %s: %w", sweepID, err)
	}
	defer rows.Close()

	var out []*Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

// ListRunsForSweep returns every run (including terminal, non-terminal,
// orphaned, and failed ones) across all of the sweep's test points, ordered
// by created_at. Used to compute per-test-point run durations for status's
// ETA estimate, avoiding an N+1 query per test point.
func (s *Store) ListRunsForSweep(sweepID string) ([]*Run, error) {
	rows, err := s.db.Query(
		`SELECT r.run_id, r.test_point_id, r.iteration_attempt, r.status, r.outcome, r.local_artifact_dir, r.fetch_attempts, r.created_at, r.updated_at
		 FROM runs r JOIN test_points tp ON tp.id = r.test_point_id
		 WHERE tp.sweep_id = ?
		 ORDER BY r.created_at`,
		sweepID,
	)
	if err != nil {
		return nil, fmt.Errorf("sweepstate: list runs for sweep %s: %w", sweepID, err)
	}
	defer rows.Close()

	var out []*Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

// ListRunsForTestPoint returns every run (including non-terminal, orphaned,
// and failed ones) for a single test point, ordered by iteration attempt.
// Unlike ListNonTerminalRuns, this includes terminal runs -- callers wanting
// only successful, artifact-bearing runs (e.g. `dbarenactl results`) filter
// on Outcome/LocalArtifactDir themselves.
func (s *Store) ListRunsForTestPoint(testPointID string) ([]*Run, error) {
	rows, err := s.db.Query(
		`SELECT run_id, test_point_id, iteration_attempt, status, outcome, local_artifact_dir, fetch_attempts, created_at, updated_at
		 FROM runs WHERE test_point_id = ? ORDER BY iteration_attempt`,
		testPointID,
	)
	if err != nil {
		return nil, fmt.Errorf("sweepstate: list runs for test point %s: %w", testPointID, err)
	}
	defer rows.Close()

	var out []*Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func scanRun(r rowScanner) (*Run, error) {
	var run Run
	var status string
	if err := r.Scan(&run.RunID, &run.TestPointID, &run.IterationAttempt, &status, &run.Outcome,
		&run.LocalArtifactDir, &run.FetchAttempts, &run.CreatedAt, &run.UpdatedAt); err != nil {
		return nil, fmt.Errorf("sweepstate: scan run: %w", err)
	}
	run.Status = RunStatus(status)
	return &run, nil
}

// SetRunStatus transitions a run to a new status with no other side effects.
// Use the Finalize* methods instead when the transition also needs to update
// the parent test point's counters.
func (s *Store) SetRunStatus(runID string, status RunStatus) error {
	res, err := s.db.Exec(`UPDATE runs SET status = ?, updated_at = ? WHERE run_id = ?`, string(status), time.Now().UTC(), runID)
	if err != nil {
		return fmt.Errorf("sweepstate: set run %s status: %w", runID, err)
	}
	return checkOneRowAffected(res, "run", runID)
}

// SetRunOutcome records the known outcome ("success" or "failure") for a run
// that has finished on the remote side but not yet been finalized locally.
func (s *Store) SetRunOutcome(runID, outcome string) error {
	res, err := s.db.Exec(`UPDATE runs SET outcome = ?, updated_at = ? WHERE run_id = ?`, outcome, time.Now().UTC(), runID)
	if err != nil {
		return fmt.Errorf("sweepstate: set run %s outcome: %w", runID, err)
	}
	return checkOneRowAffected(res, "run", runID)
}

// SetRunArtifactDir records where a run's fetched artifacts landed locally.
func (s *Store) SetRunArtifactDir(runID, dir string) error {
	res, err := s.db.Exec(`UPDATE runs SET local_artifact_dir = ?, updated_at = ? WHERE run_id = ?`, dir, time.Now().UTC(), runID)
	if err != nil {
		return fmt.Errorf("sweepstate: set run %s artifact dir: %w", runID, err)
	}
	return checkOneRowAffected(res, "run", runID)
}

// IncrementFetchAttempts increments and returns a run's results-pull retry
// counter, used to bound local retries before escalating to stop-the-world.
func (s *Store) IncrementFetchAttempts(runID string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("sweepstate: begin increment fetch attempts: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var n int
	if err := tx.QueryRow(`SELECT fetch_attempts FROM runs WHERE run_id = ?`, runID).Scan(&n); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("sweepstate: read fetch attempts for %s: %w", runID, err)
	}
	n++
	if _, err := tx.Exec(`UPDATE runs SET fetch_attempts = ?, updated_at = ? WHERE run_id = ?`, n, time.Now().UTC(), runID); err != nil {
		return 0, fmt.Errorf("sweepstate: write fetch attempts for %s: %w", runID, err)
	}
	return n, tx.Commit()
}

// FinalizeRun marks a run terminal (done or failed, per outcome) and, in the
// same transaction, increments its test point's matching counter -- unless
// the test point is already Skipped, in which case the counter is left
// alone. This is the one place SuccessesCount/FailuresCount change, so a
// crash can never leave a run terminal without its counter bumped or vice
// versa.
//
// A test point can have more than one concurrent attempt in flight, so a
// straggler run can still be non-terminal (and in need of teardown) at the
// moment its test point gets skipped. That straggler must still be
// reconciled to terminal -- its environment still needs to come down -- but
// its eventual outcome no longer means anything once the test point has
// been given up on, and counting it would otherwise let
// successes/failures drift past what the user actually saw when they made
// the skip decision (e.g. failures ending up above the failure budget).
func (s *Store) FinalizeRun(runID, outcome string) error {
	if outcome != "success" && outcome != "failure" {
		return fmt.Errorf("sweepstate: finalize run %s: invalid outcome %q", runID, outcome)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("sweepstate: begin finalize run %s: %w", runID, err)
	}
	defer tx.Rollback() //nolint:errcheck

	var testPointID string
	if err := tx.QueryRow(`SELECT test_point_id FROM runs WHERE run_id = ?`, runID).Scan(&testPointID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("sweepstate: finalize run %s: %w", runID, err)
	}
	var skipped int
	if err := tx.QueryRow(`SELECT skipped FROM test_points WHERE id = ?`, testPointID).Scan(&skipped); err != nil {
		return fmt.Errorf("sweepstate: finalize run %s: read test point %s: %w", runID, testPointID, err)
	}

	finalStatus := RunDone
	counterColumn := "successes_count"
	if outcome == "failure" {
		finalStatus = RunFailed
		counterColumn = "failures_count"
	}

	now := time.Now().UTC()
	if _, err := tx.Exec(`UPDATE runs SET status = ?, outcome = ?, updated_at = ? WHERE run_id = ?`,
		string(finalStatus), outcome, now, runID); err != nil {
		return fmt.Errorf("sweepstate: finalize run %s: %w", runID, err)
	}
	if skipped == 0 {
		// counterColumn is one of two fixed, non-user-controlled literals
		// above, not user input -- safe to interpolate into the statement
		// text.
		if _, err := tx.Exec(`UPDATE test_points SET `+counterColumn+` = `+counterColumn+` + 1 WHERE id = ?`, testPointID); err != nil {
			return fmt.Errorf("sweepstate: increment %s for test point %s: %w", counterColumn, testPointID, err)
		}
	}
	return tx.Commit()
}

func checkOneRowAffected(res sql.Result, kind, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sweepstate: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("sweepstate: %s %s: %w", kind, id, ErrNotFound)
	}
	return nil
}

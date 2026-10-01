package sweepstate

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS sweeps (
	id           TEXT PRIMARY KEY,
	provider     TEXT NOT NULL,
	product      TEXT NOT NULL DEFAULT '',
	plan         TEXT NOT NULL DEFAULT '',
	workload     TEXT NOT NULL,
	params_json  TEXT NOT NULL,
	status       TEXT NOT NULL,
	error_action TEXT NOT NULL DEFAULT '',
	error_target TEXT NOT NULL DEFAULT '',
	error_detail TEXT NOT NULL DEFAULT '',
	error_at     DATETIME,
	created_at   DATETIME NOT NULL,
	last_started_at DATETIME
);

CREATE TABLE IF NOT EXISTS test_points (
	id                TEXT PRIMARY KEY,
	sweep_id          TEXT NOT NULL REFERENCES sweeps(id),
	tier              TEXT NOT NULL,
	workload          TEXT NOT NULL,
	scenario          TEXT NOT NULL,
	bound_type        TEXT NOT NULL,
	variant           TEXT NOT NULL DEFAULT '',
	set_json          TEXT NOT NULL DEFAULT '{}',
	successes_needed  INTEGER NOT NULL,
	successes_count   INTEGER NOT NULL DEFAULT 0,
	failures_count    INTEGER NOT NULL DEFAULT 0,
	failure_budget    INTEGER NOT NULL,
	skipped           INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_test_points_sweep ON test_points(sweep_id);

CREATE TABLE IF NOT EXISTS runs (
	run_id             TEXT PRIMARY KEY,
	test_point_id      TEXT NOT NULL REFERENCES test_points(id),
	iteration_attempt  INTEGER NOT NULL,
	status             TEXT NOT NULL,
	outcome            TEXT NOT NULL DEFAULT '',
	local_artifact_dir TEXT NOT NULL DEFAULT '',
	fetch_attempts     INTEGER NOT NULL DEFAULT 0,
	created_at         DATETIME NOT NULL,
	updated_at         DATETIME NOT NULL,
	provisioned_at     DATETIME
);
CREATE INDEX IF NOT EXISTS idx_runs_test_point ON runs(test_point_id);
`

// Store is dbarenactl's local SQLite-backed state store.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and ensures
// its schema exists. WAL mode is enabled so concurrent readers (e.g.
// `dbarenactl status` while a `dbarenactl run` is active) don't block the writer.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("sweepstate: mkdir %s: %w", dir, err)
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("sweepstate: open %s: %w", path, err)
	}
	// SQLite only allows one writer at a time regardless of driver-level
	// pooling; forcing a single connection avoids "database is locked"
	// errors from this process racing itself across goroutines.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("sweepstate: enable WAL: %w", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=ON;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("sweepstate: enable foreign keys: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("sweepstate: create schema: %w", err)
	}
	if err := addSkippedColumnIfMissing(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := addLastStartedAtColumnIfMissing(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := addProvisionedAtColumnIfMissing(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// addSkippedColumnIfMissing retrofits test_points.skipped onto a database
// created before that column existed. CREATE TABLE IF NOT EXISTS (see
// schema above) only takes effect for brand-new databases, so an existing
// on-disk database needs this explicit, idempotent migration every time it's
// opened.
func addSkippedColumnIfMissing(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(test_points)`)
	if err != nil {
		return fmt.Errorf("sweepstate: inspect test_points schema: %w", err)
	}
	defer rows.Close()

	hasSkipped := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return fmt.Errorf("sweepstate: inspect test_points schema: %w", err)
		}
		if name == "skipped" {
			hasSkipped = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sweepstate: inspect test_points schema: %w", err)
	}
	if hasSkipped {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE test_points ADD COLUMN skipped INTEGER NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("sweepstate: add test_points.skipped column: %w", err)
	}
	return nil
}

// addLastStartedAtColumnIfMissing retrofits sweeps.last_started_at onto a
// database created before that column existed, backfilling it from
// created_at so existing rows sort and display sensibly. Like
// addSkippedColumnIfMissing, this runs every time the database is opened
// since CREATE TABLE IF NOT EXISTS only takes effect for brand-new databases.
func addLastStartedAtColumnIfMissing(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(sweeps)`)
	if err != nil {
		return fmt.Errorf("sweepstate: inspect sweeps schema: %w", err)
	}
	defer rows.Close()

	hasLastStartedAt := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return fmt.Errorf("sweepstate: inspect sweeps schema: %w", err)
		}
		if name == "last_started_at" {
			hasLastStartedAt = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sweepstate: inspect sweeps schema: %w", err)
	}
	if hasLastStartedAt {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE sweeps ADD COLUMN last_started_at DATETIME`); err != nil {
		return fmt.Errorf("sweepstate: add sweeps.last_started_at column: %w", err)
	}
	if _, err := db.Exec(`UPDATE sweeps SET last_started_at = created_at WHERE last_started_at IS NULL`); err != nil {
		return fmt.Errorf("sweepstate: backfill sweeps.last_started_at: %w", err)
	}
	return nil
}

// addProvisionedAtColumnIfMissing retrofits runs.provisioned_at onto a
// database created before that column existed. The column is nullable and
// every query names its columns explicitly, so a dbarenactl process built
// before this migration keeps working against the migrated database; its
// runs just leave the column NULL.
func addProvisionedAtColumnIfMissing(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(runs)`)
	if err != nil {
		return fmt.Errorf("sweepstate: inspect runs schema: %w", err)
	}
	defer rows.Close()

	hasProvisionedAt := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return fmt.Errorf("sweepstate: inspect runs schema: %w", err)
		}
		if name == "provisioned_at" {
			hasProvisionedAt = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sweepstate: inspect runs schema: %w", err)
	}
	if hasProvisionedAt {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE runs ADD COLUMN provisioned_at DATETIME`); err != nil {
		return fmt.Errorf("sweepstate: add runs.provisioned_at column: %w", err)
	}
	return nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error { return s.db.Close() }

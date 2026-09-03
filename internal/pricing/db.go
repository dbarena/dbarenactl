package pricing

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS pricing_snapshots (
	id                TEXT PRIMARY KEY,
	provider          TEXT NOT NULL,
	product           TEXT NOT NULL DEFAULT '',
	plan              TEXT NOT NULL DEFAULT '',
	region            TEXT NOT NULL DEFAULT '',
	method            TEXT NOT NULL,
	source            TEXT NOT NULL DEFAULT '',
	currency          TEXT NOT NULL DEFAULT 'USD',
	fetched_at        DATETIME NOT NULL,
	source_updated_at DATETIME,
	item_count        INTEGER NOT NULL DEFAULT 0,
	items_json        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_pricing_snapshots_provider_fetched
	ON pricing_snapshots(provider, fetched_at DESC);
`

// Store is dbarenactl's local SQLite-backed pricing cache.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and ensures
// its schema exists. Mirrors internal/sweepstate.Open exactly, against the
// same physical file but a disjoint table set -- pricing has its own
// lifecycle independent of sweeps.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("pricing: mkdir %s: %w", dir, err)
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("pricing: open %s: %w", path, err)
	}
	// SQLite only allows one writer at a time regardless of driver-level
	// pooling; forcing a single connection avoids "database is locked"
	// errors from this process racing itself across goroutines.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("pricing: enable WAL: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("pricing: create schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error { return s.db.Close() }

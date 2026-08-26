package pricing

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned by Get* methods when no row matches.
var ErrNotFound = errors.New("pricing: not found")

const selectColumns = `id, provider, product, plan, region, method, source, currency, fetched_at, source_updated_at, item_count, items_json`

// CreateSnapshot inserts snap as a new, immutable row. Callers set snap.ID
// (uuid.NewString()) and snap.FetchedAt before calling.
func (s *Store) CreateSnapshot(snap *Snapshot) error {
	itemsJSON, err := json.Marshal(snap.Items)
	if err != nil {
		return fmt.Errorf("pricing: marshal items for %s: %w", snap.ID, err)
	}
	_, err = s.db.Exec(
		`INSERT INTO pricing_snapshots (id, provider, product, plan, region, method, source, currency, fetched_at, source_updated_at, item_count, items_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		snap.ID, snap.Provider, snap.Product, snap.Plan, snap.Region, string(snap.Method), snap.Source, snap.Currency,
		snap.FetchedAt.UTC(), nullableTime(snap.SourceUpdatedAt), len(snap.Items), string(itemsJSON),
	)
	if err != nil {
		return fmt.Errorf("pricing: create snapshot %s: %w", snap.ID, err)
	}
	return nil
}

// GetSnapshot loads one snapshot by id.
func (s *Store) GetSnapshot(id string) (*Snapshot, error) {
	row := s.db.QueryRow(`SELECT `+selectColumns+` FROM pricing_snapshots WHERE id = ?`, id)
	snap, err := scanSnapshot(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return snap, err
}

// GetLatestSnapshot returns the most recently fetched/set snapshot for the
// exact (provider, product, plan, region) combination (ErrNotFound if none
// exists).
func (s *Store) GetLatestSnapshot(provider, product, plan, region string) (*Snapshot, error) {
	row := s.db.QueryRow(
		`SELECT `+selectColumns+` FROM pricing_snapshots WHERE provider = ? AND product = ? AND plan = ? AND region = ? ORDER BY fetched_at DESC LIMIT 1`,
		provider, product, plan, region,
	)
	snap, err := scanSnapshot(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return snap, err
}

// ListSnapshots returns every snapshot for provider (all regions), newest
// first.
func (s *Store) ListSnapshots(provider string) ([]*Snapshot, error) {
	rows, err := s.db.Query(
		`SELECT `+selectColumns+` FROM pricing_snapshots WHERE provider = ? ORDER BY fetched_at DESC`, provider,
	)
	if err != nil {
		return nil, fmt.Errorf("pricing: list snapshots for %s: %w", provider, err)
	}
	defer rows.Close()
	return scanSnapshots(rows)
}

// ListLatestSnapshots returns the latest snapshot for every distinct
// (provider, product, plan, region) combination, optionally restricted to
// one provider ("" means every provider), ordered by provider/product/plan/
// region. The key is widened beyond (provider, region) because e.g. GCP's
// Enterprise and Enterprise Plus editions have genuinely different prices
// and must never shadow each other as "latest".
func (s *Store) ListLatestSnapshots(provider string) ([]*Snapshot, error) {
	query := `
		SELECT ` + selectColumnsPrefixed("ps") + `
		FROM pricing_snapshots ps
		JOIN (
			SELECT provider, product, plan, region, MAX(fetched_at) AS max_fetched_at
			FROM pricing_snapshots`
	var args []any
	if provider != "" {
		query += ` WHERE provider = ?`
		args = append(args, provider)
	}
	query += `
			GROUP BY provider, product, plan, region
		) latest ON latest.provider = ps.provider AND latest.product = ps.product AND latest.plan = ps.plan
			AND latest.region = ps.region AND latest.max_fetched_at = ps.fetched_at
		ORDER BY ps.provider, ps.product, ps.plan, ps.region`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("pricing: list latest snapshots: %w", err)
	}
	defer rows.Close()
	return scanSnapshots(rows)
}

// ListAllSnapshots returns every snapshot, optionally restricted to one
// provider ("" means every provider), ordered by provider then newest
// first.
func (s *Store) ListAllSnapshots(provider string) ([]*Snapshot, error) {
	query := `SELECT ` + selectColumns + ` FROM pricing_snapshots`
	var args []any
	if provider != "" {
		query += ` WHERE provider = ?`
		args = append(args, provider)
	}
	query += ` ORDER BY provider, fetched_at DESC`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("pricing: list all snapshots: %w", err)
	}
	defer rows.Close()
	return scanSnapshots(rows)
}

type rowScanner interface {
	Scan(dest ...any) error
}

// selectColumnsPrefixed returns selectColumns with each column prefixed by
// "<alias>.", for use in queries that join pricing_snapshots against itself.
func selectColumnsPrefixed(alias string) string {
	cols := []string{"id", "provider", "product", "plan", "region", "method", "source", "currency", "fetched_at", "source_updated_at", "item_count", "items_json"}
	out := ""
	for i, c := range cols {
		if i > 0 {
			out += ", "
		}
		out += alias + "." + c
	}
	return out
}

func scanSnapshots(rows *sql.Rows) ([]*Snapshot, error) {
	var out []*Snapshot
	for rows.Next() {
		snap, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, snap)
	}
	return out, rows.Err()
}

func scanSnapshot(r rowScanner) (*Snapshot, error) {
	var snap Snapshot
	var method string
	var sourceUpdatedAt sql.NullTime
	var itemCount int
	var itemsJSON string
	if err := r.Scan(&snap.ID, &snap.Provider, &snap.Product, &snap.Plan, &snap.Region, &method, &snap.Source, &snap.Currency,
		&snap.FetchedAt, &sourceUpdatedAt, &itemCount, &itemsJSON); err != nil {
		return nil, fmt.Errorf("pricing: scan snapshot: %w", err)
	}
	snap.Method = Method(method)
	if sourceUpdatedAt.Valid {
		snap.SourceUpdatedAt = &sourceUpdatedAt.Time
	}
	if err := json.Unmarshal([]byte(itemsJSON), &snap.Items); err != nil {
		return nil, fmt.Errorf("pricing: unmarshal items for %s: %w", snap.ID, err)
	}
	return &snap, nil
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

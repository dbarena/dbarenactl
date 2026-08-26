// Package pricing is dbarenactl's local cache of provider on-demand list
// pricing. It stores immutable, timestamped snapshots locally so a benchmark's
// cost can always be reconstructed against the pricing that was in effect
// when it ran. Storage is intentionally provider-agnostic.
package pricing

import "time"

// Method records how a snapshot was captured.
type Method string

const (
	MethodFetch Method = "fetch" // via a Fetcher hitting a primary-source API
	MethodSet   Method = "set"   // manually supplied by an operator
)

// Item is one priced line (one SKU/rate) within a snapshot. Fields are
// deliberately generic -- Attributes carries whatever provider-specific
// dimensions matter (instance type, disk type, database engine, ...) as
// free-form key/value pairs, so Item never needs new fields as providers are
// added.
type Item struct {
	SKU         string            `json:"sku,omitempty"`
	Description string            `json:"description"`
	Unit        string            `json:"unit"`
	PriceUSD    float64           `json:"price_usd"`
	Region      string            `json:"region,omitempty"`
	Attributes  map[string]string `json:"attributes,omitempty"`
}

// Snapshot is one immutable pricing capture for one (provider, product,
// plan, region) combination. History is kept: every fetch/set inserts a new
// Snapshot row, never overwrites one even when a re-fetch returns unchanged
// prices. Provider/Product/Plan mirror internal/manifest.Manifest's fields
// directly (e.g. "AWS"/"RDS"/"", or "GCP"/"Cloud SQL for Postgres"/
// "Enterprise Plus") rather than being flattened into one composite string.
type Snapshot struct {
	ID       string // uuid, assigned by the caller before CreateSnapshot
	Provider string // e.g. "AWS", "GCP", "Supabase"
	Product  string // e.g. "RDS", "Cloud SQL for Postgres", "Supabase"
	Plan     string // e.g. "Enterprise Plus", "Pro"; "" if the provider has no plan concept
	Region   string // "" if not region-scoped (e.g. supabase)
	Method   Method
	Source   string // human-readable provenance, e.g. API name + region
	Currency string // "USD"

	FetchedAt       time.Time  // when dbarenactl captured this (UTC)
	SourceUpdatedAt *time.Time // provider's own last-price-update time, nil if the source exposes none

	Items []Item
}

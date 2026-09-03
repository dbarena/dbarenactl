package pricing

import (
	"context"
	"time"
)

// FetchResult is what a Fetcher returns for one Fetch call.
type FetchResult struct {
	Items []Item
	// SourceUpdatedAt is the provider's own last-price-update timestamp, if
	// its source exposes one (GCP: max SKU effectiveTime; AWS: the offer
	// file's publicationDate). Nil if unknown.
	SourceUpdatedAt *time.Time
	// Source is a human-readable description of the primary source used,
	// e.g. "GCP Cloud Billing Catalog API (Cloud SQL service, region us-east1)".
	Source string
}

// Fetcher retrieves a current on-demand (list, non-discounted) pricing
// snapshot from one provider's primary, machine-readable source. One
// Fetcher instance may back more than one provider id -- e.g. gcp/cloudsql
// and gcp/cloudsql-enterprise-plus price from the same Cloud SQL Billing
// Catalog SKUs and share a single Fetcher registered under both ids.
type Fetcher interface {
	// Fetch retrieves pricing for providerID (as registered) and region.
	// region is always resolved by the caller (from a candidate manifest)
	// before this is called -- Fetch implementations must not guess a
	// default. Implementations must return only undiscounted, on-demand
	// list prices.
	Fetch(ctx context.Context, providerID, region string) (*FetchResult, error)
}

// Registry maps provider ids to the Fetcher that serves them. It holds no
// provider-specific logic itself -- it is populated by cmd/dbarenactl, the
// only place in the binary allowed to import provider packages.
type Registry struct {
	fetchers map[string]Fetcher
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry { return &Registry{fetchers: map[string]Fetcher{}} }

// Register associates fetcher with every id in providerIDs.
func (r *Registry) Register(fetcher Fetcher, providerIDs ...string) {
	for _, id := range providerIDs {
		r.fetchers[id] = fetcher
	}
}

// Lookup returns the Fetcher for providerID, if any is registered.
func (r *Registry) Lookup(providerID string) (Fetcher, bool) {
	f, ok := r.fetchers[providerID]
	return f, ok
}

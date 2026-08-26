package main

import (
	"github.com/dbarena/dbarenactl/internal/pricing"
	"github.com/dbarena/dbarenactl/internal/pricing/providers/aws"
	"github.com/dbarena/dbarenactl/internal/pricing/providers/gcp"
	"github.com/dbarena/dbarenactl/internal/pricing/providers/supabase"
)

// newPricingRegistry builds the registry of real, automated Fetchers, keyed
// by canonical provider id. This is the only file in cmd/dbarenactl allowed
// to import internal/pricing/providers/* -- everything else (list, show,
// and any future results command) only ever touches the generic
// internal/pricing.Store.
//
// Keyed by internal/manifest's Provider enum (via Manifest.PricingFetcherKey,
// which deliberately ignores Plan): a candidate whose Plan selects a
// different edition of the same Provider+Product -- e.g. GCP's Enterprise
// vs. Enterprise Plus, both priced from the exact same Cloud SQL Billing
// Catalog SKUs -- reuses the one entry here automatically. This registry
// only needs a new entry when a manifest introduces a genuinely new
// Provider or Product, never for a new Plan or a new candidate file.
func newPricingRegistry() *pricing.Registry {
	reg := pricing.NewRegistry()
	reg.Register(gcp.New(), "gcp/cloudsql")
	reg.Register(aws.New(), "aws/rds")
	reg.Register(supabase.New(), "supabase")
	return reg
}

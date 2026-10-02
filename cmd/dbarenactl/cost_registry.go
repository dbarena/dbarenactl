package main

import (
	"github.com/dbarena/dbarenactl/internal/pricing"
	"github.com/dbarena/dbarenactl/internal/pricing/providers/aws"
	"github.com/dbarena/dbarenactl/internal/pricing/providers/gcp"
	"github.com/dbarena/dbarenactl/internal/pricing/providers/supabase"
)

// costCalculators maps the same provider ids as newPricingRegistry (see
// pricing_registry.go for why Plan is deliberately excluded) to the
// Calculator that turns a fetched Snapshot's Items into a monthly cost
// estimate. This is the only other file in cmd/dbarenactl allowed to import
// internal/pricing/providers/* -- `results` only ever touches this map, the
// vcpuRAMFuncs map below, and the generic internal/pricing.Calculator
// interface.
func newCostCalculators() map[string]pricing.Calculator {
	return map[string]pricing.Calculator{
		"gcp/cloudsql": gcp.Calculator{},
		"aws/rds":      aws.Calculator{},
		"supabase":     supabase.Calculator{},
	}
}

// vcpuRAMFunc derives (vcpu, ram_gb) for one instance type from a pricing
// snapshot's items -- AWS reads it off the matched item's own attributes;
// GCP parses it out of the tier string and ignores items entirely. Supabase
// is deliberately absent: its vcpu/ram_gb come from each run's own
// diagnostics/addons.json instead (see supabaseComputeSize), not from a
// pricing snapshot -- supabase.com/pricing.md's Compute Add-Ons table no
// longer publishes a parseable vCPU count for every tier (shared-compute
// sizes list "Shared compute" with no number).
type vcpuRAMFunc func(items []pricing.Item, instanceType string) (vcpu, ramGB float64, err error)

func newVCPURAMFuncs() map[string]vcpuRAMFunc {
	return map[string]vcpuRAMFunc{
		"gcp/cloudsql": gcp.VCPUAndRAMGB,
		"aws/rds":      aws.VCPUAndRAMGB,
	}
}

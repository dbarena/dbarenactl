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

// vcpuRAMFunc derives (vcpu, ram_gb) for one instance type/project_size from
// a pricing snapshot's items -- AWS and Supabase read it off the matched
// item's own attributes; GCP parses it out of the tier string and ignores
// items entirely. Unified into one signature so `results` can call whichever
// applies without a type switch.
type vcpuRAMFunc func(items []pricing.Item, instanceType string) (vcpu, ramGB float64, err error)

func newVCPURAMFuncs() map[string]vcpuRAMFunc {
	return map[string]vcpuRAMFunc{
		"gcp/cloudsql": gcp.VCPUAndRAMGB,
		"aws/rds":      aws.VCPUAndRAMGB,
		"supabase":     supabase.VCPUAndRAMGB,
	}
}

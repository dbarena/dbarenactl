package main

import (
	"testing"

	"github.com/dbarena/dbarenactl/internal/manifest"
)

// TestResolvePricingInputs_ReadsFromPricingBlock is a regression test for the
// 2026-09-01 bug: db_instance_type/disk_type/disk_baseline_iops/
// disk_baseline_throughput_mibps must be read from TestPointDef.Pricing, not
// TestPointDef.Set (Set's keys are forwarded verbatim to benchctl as --set
// flags, and none of these four are benchctl scenario inputs).
func TestResolvePricingInputs_ReadsFromPricingBlock(t *testing.T) {
	def := &manifest.TestPointDef{
		Set: map[string]string{
			"project_size":          "xlarge",
			"disk_size_gb":          "400",
			"disk_iops":             "12000",
			"disk_throughput_mibps": "500",
			"warehouses":            "640",
		},
		Pricing: map[string]string{
			"db_instance_type":               "db.m6g.xlarge",
			"disk_type":                      "gp3",
			"disk_baseline_iops":             "12000",
			"disk_baseline_throughput_mibps": "500",
		},
	}

	pi := resolvePricingInputs(def)

	if pi.instanceType != "db.m6g.xlarge" {
		t.Errorf("instanceType = %q, want %q", pi.instanceType, "db.m6g.xlarge")
	}
	if pi.diskType != "gp3" {
		t.Errorf("diskType = %q, want %q", pi.diskType, "gp3")
	}
	if pi.diskBaselineIOPS == nil || *pi.diskBaselineIOPS != 12000 {
		t.Errorf("diskBaselineIOPS = %v, want 12000", pi.diskBaselineIOPS)
	}
	if pi.diskBaselineThroughput == nil || *pi.diskBaselineThroughput != 500 {
		t.Errorf("diskBaselineThroughput = %v, want 500", pi.diskBaselineThroughput)
	}
	// disk_size_gb/disk_iops/disk_throughput_mibps/warehouses are real
	// benchctl inputs and must still come from Set, unaffected by this fix.
	if pi.diskGB == nil || *pi.diskGB != 400 {
		t.Errorf("diskGB = %v, want 400", pi.diskGB)
	}
	if pi.iops == nil || *pi.iops != 12000 {
		t.Errorf("iops = %v, want 12000", pi.iops)
	}
	if pi.throughputMbps == nil || *pi.throughputMbps != 500 {
		t.Errorf("throughputMbps = %v, want 500", pi.throughputMbps)
	}
	if pi.warehouses == nil || *pi.warehouses != 640 {
		t.Errorf("warehouses = %v, want 640", pi.warehouses)
	}
}

// TestResolvePricingInputs_FallsBackToProjectSizeWithoutPricingBlock covers
// Supabase's test points, which have no pricing: block at all (project_size
// doubles as its own compute SKU, and it has no db_instance_type or
// disk-baseline-override concept).
func TestResolvePricingInputs_FallsBackToProjectSizeWithoutPricingBlock(t *testing.T) {
	def := &manifest.TestPointDef{
		Set: map[string]string{
			"project_size": "xlarge",
			"disk_iops":    "12000",
		},
	}

	pi := resolvePricingInputs(def)

	if pi.instanceType != "xlarge" {
		t.Errorf("instanceType = %q, want %q (fallback to project_size)", pi.instanceType, "xlarge")
	}
	if pi.diskType != "" {
		t.Errorf("diskType = %q, want empty", pi.diskType)
	}
	if pi.diskBaselineIOPS != nil {
		t.Errorf("diskBaselineIOPS = %v, want nil", pi.diskBaselineIOPS)
	}
	if pi.diskBaselineThroughput != nil {
		t.Errorf("diskBaselineThroughput = %v, want nil", pi.diskBaselineThroughput)
	}
}

// TestResolvePricingInputs_PricingNeverLeaksSetOnlyKeys guards the other
// direction of the bug: a db_instance_type placed in Set (as every candidate
// file used to do) must NOT be picked up here -- it has to move to Pricing,
// or resolvePricingInputs falls back to project_size instead, exactly as it
// would for a manifest that never had the field at all.
func TestResolvePricingInputs_PricingNeverLeaksSetOnlyKeys(t *testing.T) {
	def := &manifest.TestPointDef{
		Set: map[string]string{
			"project_size":     "xlarge",
			"db_instance_type": "db.m6g.xlarge", // stale location; must be ignored
		},
	}

	pi := resolvePricingInputs(def)

	if pi.instanceType != "xlarge" {
		t.Errorf("instanceType = %q, want %q -- db_instance_type in Set must not be read", pi.instanceType, "xlarge")
	}
}

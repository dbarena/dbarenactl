package gcp

import (
	"math"
	"strings"
	"testing"

	"github.com/dbarena/dbarenactl/internal/pricing"
)

const epsilon = 1e-9

func approxEqual(a, b float64) bool { return math.Abs(a-b) < epsilon }

// enterpriseN4ItemSet mirrors the real Cloud Billing Catalog rows for an
// Enterprise/N4/Hyperdisk-Balanced tier (e.g. db-custom-N4-2-4096), as
// fetched for region us-east1.
func enterpriseN4ItemSet() []pricing.Item {
	return []pricing.Item{
		{SKU: "VCPU", Description: "Cloud SQL for Postgres: Zonal - Enterprise N4 vCPU in South Carolina", PriceUSD: 0.0413},
		{SKU: "RAM", Description: "Cloud SQL for Postgres: Zonal- Enterprise N4 RAM in South Carolina", PriceUSD: 0.007},
		{SKU: "STORAGE", Description: "Cloud SQL for Postgres: Zonal - Enterprise Storage Hyperdisk Balanced Capacity in South Carolina", PriceUSD: 0.115},
		{SKU: "REGIONAL-STORAGE", Description: "Cloud SQL for Postgres: Zonal - Regional Enterprise Storage Hyperdisk Balanced Capacity in South Carolina", PriceUSD: 0.23},
		{SKU: "IOPS", Description: "Cloud SQL for Postgres: Zonal - Enterprise Storage Hyperdisk Balanced IOPS in South Carolina", PriceUSD: 0.02},
		{SKU: "THROUGHPUT", Description: "Cloud SQL for Postgres: Zonal - Enterprise Storage Hyperdisk Balanced Throughput in South Carolina", PriceUSD: 0.08},
	}
}

// enterprisePlusNItemSet mirrors the real rows for an Enterprise
// Plus/N/perf-optimized/PD-SSD tier (db-perf-optimized-N-2).
func enterprisePlusNItemSet() []pricing.Item {
	return []pricing.Item{
		{SKU: "VCPU", Description: "Cloud SQL for PostgreSQL: Zonal - Enterprise Plus N vCPU in Carolina", PriceUSD: 0.0537},
		{SKU: "RAM", Description: "Cloud SQL for PostgreSQL: Zonal - Enterprise Plus N RAM in Carolina", PriceUSD: 0.0091},
		{SKU: "STORAGE", Description: "Cloud SQL for PostgreSQL: Zonal - Enterprise Plus Standard Storage in Carolina", PriceUSD: 0.17},
	}
}

func componentUSD(bd *pricing.CostBreakdown, name string) float64 {
	for _, c := range bd.Components {
		if c.Name == name {
			return c.AmountUSD
		}
	}
	return -1
}

func TestVCPUAndRAMGB_CustomShape(t *testing.T) {
	vcpu, ramGB, err := vcpuAndRAMGB("db-custom-N4-2-4096")
	if err != nil {
		t.Fatalf("vcpuAndRAMGB: %v", err)
	}
	if vcpu != 2 || ramGB != 4 {
		t.Errorf("vcpu=%v ramGB=%v, want 2, 4", vcpu, ramGB)
	}
}

func TestVCPUAndRAMGB_PerfOptimizedShape(t *testing.T) {
	vcpu, ramGB, err := vcpuAndRAMGB("db-perf-optimized-N-2")
	if err != nil {
		t.Fatalf("vcpuAndRAMGB: %v", err)
	}
	if vcpu != 2 || ramGB != 16 {
		t.Errorf("vcpu=%v ramGB=%v, want 2, 16 (fixed 1vCPU:8GB ratio)", vcpu, ramGB)
	}
}

func TestVCPUAndRAMGB_UnrecognizedShape_Errors(t *testing.T) {
	if _, _, err := vcpuAndRAMGB("some-unknown-shape-9"); err == nil {
		t.Fatal("expected an error for an unrecognized tier shape, not a silent zero")
	}
}

func TestParseTier_CustomShape_ExtractsEditionAndFamily(t *testing.T) {
	pt, err := parseTier("db-custom-N4-2-4096")
	if err != nil {
		t.Fatalf("parseTier: %v", err)
	}
	if pt.Edition != editionEnterprise || pt.Family != "N4" {
		t.Errorf("edition=%v family=%v, want %v N4", pt.Edition, pt.Family, editionEnterprise)
	}
}

func TestParseTier_PerfOptimizedShape_ExtractsEditionAndFamily(t *testing.T) {
	pt, err := parseTier("db-perf-optimized-N-2")
	if err != nil {
		t.Fatalf("parseTier: %v", err)
	}
	if pt.Edition != editionEnterprisePlus || pt.Family != "N" {
		t.Errorf("edition=%v family=%v, want %v N", pt.Edition, pt.Family, editionEnterprisePlus)
	}
}

func TestCost_ComputeAndStorage_NoOverageWithoutBaseline(t *testing.T) {
	c := Calculator{}
	bd, err := c.Cost(enterpriseN4ItemSet(), pricing.CostInput{
		InstanceType: "db-custom-N4-2-4096", DiskGB: 32, DiskType: "HYPERDISK_BALANCED",
	})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	wantVCPU := 0.0413 * 2 * pricing.HoursPerMonth
	wantRAM := 0.007 * 4 * pricing.HoursPerMonth
	wantStorage := 0.115 * 32
	if got := componentUSD(bd, "compute_vcpu"); !approxEqual(got, wantVCPU) {
		t.Errorf("compute_vcpu = %v, want %v", got, wantVCPU)
	}
	if got := componentUSD(bd, "compute_ram"); !approxEqual(got, wantRAM) {
		t.Errorf("compute_ram = %v, want %v", got, wantRAM)
	}
	if got := componentUSD(bd, "storage"); !approxEqual(got, wantStorage) {
		t.Errorf("storage = %v, want %v", got, wantStorage)
	}
	// No IOPS/throughput requested and no baseline set -- overage must be 0
	// and no IOPS/throughput SKU lookup should even be attempted.
	if got := componentUSD(bd, "iops_overage"); got != 0 {
		t.Errorf("iops_overage = %v, want 0", got)
	}
	if got := componentUSD(bd, "throughput_overage"); got != 0 {
		t.Errorf("throughput_overage = %v, want 0", got)
	}
}

func TestCost_EnterprisePlusN_PicksEnterprisePlusSKUsOverEnterprise(t *testing.T) {
	c := Calculator{}
	bd, err := c.Cost(enterprisePlusNItemSet(), pricing.CostInput{
		InstanceType: "db-perf-optimized-N-2", DiskGB: 128, DiskType: "PD_SSD",
	})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	wantVCPU := 0.0537 * 2 * pricing.HoursPerMonth
	wantRAM := 0.0091 * 16 * pricing.HoursPerMonth
	wantStorage := 0.17 * 128
	if got := componentUSD(bd, "compute_vcpu"); !approxEqual(got, wantVCPU) {
		t.Errorf("compute_vcpu = %v, want %v", got, wantVCPU)
	}
	if got := componentUSD(bd, "compute_ram"); !approxEqual(got, wantRAM) {
		t.Errorf("compute_ram = %v, want %v", got, wantRAM)
	}
	if got := componentUSD(bd, "storage"); !approxEqual(got, wantStorage) {
		t.Errorf("storage = %v, want %v", got, wantStorage)
	}
}

// TestCost_NoIOPSRequested_DoesNotRequireOverageSKUsToExist confirms a test
// point that doesn't request any provisioned IOPS/throughput at all never
// needs those SKUs to price it -- only a test point that actually requests
// some does.
func TestCost_NoIOPSRequested_DoesNotRequireOverageSKUsToExist(t *testing.T) {
	c := Calculator{}
	items := enterpriseN4ItemSet()[:3] // vcpu, ram, storage only -- no IOPS/throughput SKUs at all
	_, err := c.Cost(items, pricing.CostInput{InstanceType: "db-custom-N4-2-4096", DiskGB: 32, DiskType: "HYPERDISK_BALANCED"})
	if err != nil {
		t.Fatalf("Cost should not require an IOPS SKU when no IOPS are requested: %v", err)
	}
}

// TestCost_IOPSRequestedWithNoBaselineOverride_BillsInFull confirms Hyperdisk
// Balanced's real billing model: with no disk_baseline_iops override, the
// default baseline is 0 (see HyperdiskDocsURL), so requesting any IOPS at
// all bills for the full amount, not a discounted "overage above a free
// floor" the way AWS gp3 works.
func TestCost_IOPSRequestedWithNoBaselineOverride_BillsInFull(t *testing.T) {
	c := Calculator{}
	bd, err := c.Cost(enterpriseN4ItemSet(), pricing.CostInput{InstanceType: "db-custom-N4-2-4096", DiskGB: 32, DiskType: "HYPERDISK_BALANCED", IOPS: 3000})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	want := 0.02 * 3000
	if got := componentUSD(bd, "iops_overage"); !approxEqual(got, want) {
		t.Errorf("iops_overage = %v, want %v (all 3000 IOPS billed -- no free baseline by default)", got, want)
	}
}

func TestCost_IOPSOverage_BaselineFromInput(t *testing.T) {
	c := Calculator{}
	baselineIOPS := 3000.0
	bd, err := c.Cost(enterpriseN4ItemSet(), pricing.CostInput{
		InstanceType: "db-custom-N4-2-4096", DiskGB: 32, DiskType: "HYPERDISK_BALANCED", IOPS: 3500,
		DiskBaselineIOPS: &baselineIOPS,
	})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	want := 0.02 * 500
	if got := componentUSD(bd, "iops_overage"); !approxEqual(got, want) {
		t.Errorf("iops_overage = %v, want %v (500 IOPS above the stated baseline)", got, want)
	}
}

func TestCost_IOPSOverage_MissingSKU_Errors(t *testing.T) {
	c := Calculator{}
	items := enterpriseN4ItemSet()[:3] // no IOPS SKU present
	baselineIOPS := 3000.0
	_, err := c.Cost(items, pricing.CostInput{
		InstanceType: "db-custom-N4-2-4096", DiskGB: 32, DiskType: "HYPERDISK_BALANCED", IOPS: 3500,
		DiskBaselineIOPS: &baselineIOPS,
	})
	if err == nil {
		t.Fatal("expected an error: overage is requested but no IOPS SKU exists to price it")
	}
}

func TestCost_MissingVCPUSKU_Errors(t *testing.T) {
	c := Calculator{}
	items := enterpriseN4ItemSet()[1:] // drop the vCPU item
	_, err := c.Cost(items, pricing.CostInput{InstanceType: "db-custom-N4-2-4096", DiskGB: 32, DiskType: "HYPERDISK_BALANCED"})
	if err == nil {
		t.Fatal("expected an error when no vCPU rate item exists")
	}
	if !strings.Contains(err.Error(), "vCPU") {
		t.Errorf("err = %v, want it to mention the missing vCPU rate", err)
	}
}

func TestCost_RequiresInstanceType(t *testing.T) {
	c := Calculator{}
	if _, err := c.Cost(enterpriseN4ItemSet(), pricing.CostInput{DiskGB: 32, DiskType: "HYPERDISK_BALANCED"}); err == nil {
		t.Fatal("expected an error when InstanceType is empty")
	}
}

func TestCost_UnrecognizedTierShape_Errors(t *testing.T) {
	c := Calculator{}
	if _, err := c.Cost(enterpriseN4ItemSet(), pricing.CostInput{InstanceType: "totally-unknown", DiskGB: 32, DiskType: "HYPERDISK_BALANCED"}); err == nil {
		t.Fatal("expected an error for an unrecognized tier shape")
	}
}

func TestCost_MissingDiskType_Errors(t *testing.T) {
	c := Calculator{}
	_, err := c.Cost(enterpriseN4ItemSet(), pricing.CostInput{InstanceType: "db-custom-N4-2-4096", DiskGB: 32})
	if err == nil {
		t.Fatal("expected an error when DiskType is empty")
	}
	if !strings.Contains(err.Error(), "disk_type") {
		t.Errorf("err = %v, want it to mention disk_type", err)
	}
}

func TestCost_UnsupportedDiskType_Errors(t *testing.T) {
	c := Calculator{}
	_, err := c.Cost(enterpriseN4ItemSet(), pricing.CostInput{InstanceType: "db-custom-N4-2-4096", DiskGB: 32, DiskType: "PD_HDD"})
	if err == nil {
		t.Fatal("expected an error for an unsupported disk_type")
	}
	if !strings.Contains(err.Error(), "PD_HDD") {
		t.Errorf("err = %v, want it to mention the unsupported disk_type", err)
	}
}

// TestCost_N4EnterpriseDisambiguation proves the N4/Enterprise vCPU+RAM SKUs
// are picked over a same-catalog Enterprise Plus C4 decoy pair that also
// contains the bare "vcpu"/"ram" keywords -- the failure mode from the
// original bug report.
func TestCost_N4EnterpriseDisambiguation(t *testing.T) {
	items := append(enterpriseN4ItemSet(),
		pricing.Item{SKU: "DECOY-VCPU", Description: "Cloud SQL for PostgreSQL: Zonal - Enterprise Plus C4 vCPU in South Carolina", PriceUSD: 0.054},
		pricing.Item{SKU: "DECOY-RAM", Description: "Cloud SQL for PostgreSQL: Zonal - Enterprise Plus C4 RAM in South Carolina", PriceUSD: 0.009},
		pricing.Item{SKU: "DECOY-LEGACY-VCPU", Description: "Cloud SQL for PostgreSQL: Zonal - vCPU in Americas", PriceUSD: 0.0413},
	)
	c := Calculator{}
	bd, err := c.Cost(items, pricing.CostInput{InstanceType: "db-custom-N4-2-4096", DiskGB: 32, DiskType: "HYPERDISK_BALANCED"})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	wantVCPU := 0.0413 * 2 * pricing.HoursPerMonth
	if got := componentUSD(bd, "compute_vcpu"); !approxEqual(got, wantVCPU) {
		t.Errorf("compute_vcpu = %v, want %v (should pick the N4/Enterprise SKU, not a decoy)", got, wantVCPU)
	}
}

// TestCost_PerfOptimizedNEnterprisePlusDisambiguation proves the N/Enterprise
// Plus vCPU+RAM SKUs are picked over C4/C4A Enterprise Plus decoys that share
// the "Enterprise Plus"+"vcpu"/"ram" keywords.
func TestCost_PerfOptimizedNEnterprisePlusDisambiguation(t *testing.T) {
	items := append(enterprisePlusNItemSet(),
		pricing.Item{SKU: "DECOY-C4-VCPU", Description: "Cloud SQL for PostgreSQL: Zonal - Enterprise Plus C4 vCPU in South Carolina", PriceUSD: 0.054},
		pricing.Item{SKU: "DECOY-C4A-VCPU", Description: "Cloud SQL for Postgres: Zonal - Enterprise Plus Performance Optimized C4A vCPU in South Carolina", PriceUSD: 0.054},
		pricing.Item{SKU: "DECOY-C4A-RAM", Description: "Cloud SQL for PostgreSQL: Zonal - Enterprise Plus C4A RAM in South Carolina", PriceUSD: 0.009},
	)
	c := Calculator{}
	bd, err := c.Cost(items, pricing.CostInput{InstanceType: "db-perf-optimized-N-2", DiskGB: 128, DiskType: "PD_SSD"})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	wantVCPU := 0.0537 * 2 * pricing.HoursPerMonth
	wantRAM := 0.0091 * 16 * pricing.HoursPerMonth
	if got := componentUSD(bd, "compute_vcpu"); !approxEqual(got, wantVCPU) {
		t.Errorf("compute_vcpu = %v, want %v (should pick the N/Enterprise Plus SKU, not a C4/C4A decoy)", got, wantVCPU)
	}
	if got := componentUSD(bd, "compute_ram"); !approxEqual(got, wantRAM) {
		t.Errorf("compute_ram = %v, want %v (should pick the N/Enterprise Plus SKU, not a C4A decoy)", got, wantRAM)
	}
}

// TestCost_StorageExcludesIOPSAndThroughputRows proves a Hyperdisk Balanced
// IOPS/Throughput row -- which contains the literal word "storage" -- is not
// mistaken for the storage-capacity rate.
func TestCost_StorageExcludesIOPSAndThroughputRows(t *testing.T) {
	c := Calculator{}
	bd, err := c.Cost(enterpriseN4ItemSet(), pricing.CostInput{InstanceType: "db-custom-N4-2-4096", DiskGB: 32, DiskType: "HYPERDISK_BALANCED"})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	want := 0.115 * 32
	if got := componentUSD(bd, "storage"); !approxEqual(got, want) {
		t.Errorf("storage = %v, want %v (must pick Capacity, not the IOPS/Throughput rows which also contain \"storage\")", got, want)
	}
}

// TestCost_RealSnapshotRegression_MultiEditionCatalog reproduces the original
// bug report's failure mode: a single, real-shaped multi-edition/family
// catalog (both Enterprise N4 and Enterprise Plus N/C4/C4A rows, plus a
// legacy no-edition row and Enterprise Plus Data Cache rows, all present at
// once) used to make every findOneItem call ambiguous. Both candidates must
// now price successfully against it and land on the SKUs the bug report
// identified as correct.
func TestCost_RealSnapshotRegression_MultiEditionCatalog(t *testing.T) {
	items := []pricing.Item{
		{SKU: "01BB-A7A0-1F74", Description: "Cloud SQL for Postgres: Zonal - Enterprise N4 vCPU in South Carolina", PriceUSD: 0.0413},
		{SKU: "744F-0CB3-122E", Description: "Cloud SQL for Postgres: Zonal- Enterprise N4 RAM in South Carolina", PriceUSD: 0.007},
		{SKU: "1060-A18A-1D8A", Description: "Cloud SQL for Postgres: Zonal - Enterprise Storage Hyperdisk Balanced Capacity in South Carolina", PriceUSD: 0.115},
		{SKU: "2EEF-EC87-2D8A", Description: "Cloud SQL for Postgres: Zonal - Enterprise Storage Hyperdisk Balanced IOPS in South Carolina", PriceUSD: 0.02},
		{SKU: "BD59-EA22-610C", Description: "Cloud SQL for Postgres: Zonal - Enterprise Storage Hyperdisk Balanced Throughput in South Carolina", PriceUSD: 0.08},
		{SKU: "2A41-BB8F-2F73", Description: "Cloud SQL for PostgreSQL: Zonal - Enterprise Plus C4 vCPU in South Carolina", PriceUSD: 0.054},
		{SKU: "9691-70DB-9013", Description: "Cloud SQL for PostgreSQL: Zonal - Enterprise Plus C4 RAM in South Carolina", PriceUSD: 0.009},
		{SKU: "3E95-F3BF-E035", Description: "Cloud SQL for Postgres: Zonal - Enterprise Plus Performance Optimized C4A vCPU in South Carolina", PriceUSD: 0.054},
		{SKU: "252C-F783-7F43", Description: "Cloud SQL for PostgreSQL: Zonal - Enterprise Plus C4A RAM in South Carolina", PriceUSD: 0.009},
		{SKU: "E6A6-C6EB-A1D2", Description: "Cloud SQL for PostgreSQL: Zonal - Enterprise Plus N vCPU in Carolina", PriceUSD: 0.0537},
		{SKU: "793D-61D3-6B50", Description: "Cloud SQL for PostgreSQL: Zonal - Enterprise Plus N RAM in Carolina", PriceUSD: 0.0091},
		{SKU: "2794-0F6D-F3CB", Description: "Cloud SQL for PostgreSQL: Zonal - Enterprise Plus Standard Storage in Carolina", PriceUSD: 0.17},
		{SKU: "CA10-7320-43C9", Description: "Cloud SQL for PostgreSQL: Zonal - Enterprise Plus C4 Data Cache Storage in South Carolina", PriceUSD: 0.21},
		{SKU: "E9F3-FA7F-BB81", Description: "Cloud SQL for PostgreSQL: Zonal - Enterprise Plus Data Cache Storage in Carolina", PriceUSD: 0.16},
		{SKU: "965E-7C36-08E6", Description: "Cloud SQL for PostgreSQL: Zonal - vCPU in Americas", PriceUSD: 0.0413},
		{SKU: "93DA-3F55-CB04", Description: "Cloud SQL for PostgreSQL: Zonal - RAM in Americas", PriceUSD: 0.007},
	}

	c := Calculator{}

	n4bd, err := c.Cost(items, pricing.CostInput{InstanceType: "db-custom-N4-2-4096", DiskGB: 32, DiskType: "HYPERDISK_BALANCED"})
	if err != nil {
		t.Fatalf("Cost (Enterprise N4): %v", err)
	}
	if got, want := componentUSD(n4bd, "compute_vcpu"), 0.0413*2*pricing.HoursPerMonth; !approxEqual(got, want) {
		t.Errorf("Enterprise N4 compute_vcpu = %v, want %v", got, want)
	}
	if got, want := componentUSD(n4bd, "storage"), 0.115*32; !approxEqual(got, want) {
		t.Errorf("Enterprise N4 storage = %v, want %v", got, want)
	}

	plusBD, err := c.Cost(items, pricing.CostInput{InstanceType: "db-perf-optimized-N-2", DiskGB: 128, DiskType: "PD_SSD"})
	if err != nil {
		t.Fatalf("Cost (Enterprise Plus N): %v", err)
	}
	if got, want := componentUSD(plusBD, "compute_vcpu"), 0.0537*2*pricing.HoursPerMonth; !approxEqual(got, want) {
		t.Errorf("Enterprise Plus N compute_vcpu = %v, want %v", got, want)
	}
	if got, want := componentUSD(plusBD, "compute_ram"), 0.0091*16*pricing.HoursPerMonth; !approxEqual(got, want) {
		t.Errorf("Enterprise Plus N compute_ram = %v, want %v", got, want)
	}
	if got, want := componentUSD(plusBD, "storage"), 0.17*128; !approxEqual(got, want) {
		t.Errorf("Enterprise Plus N storage = %v, want %v", got, want)
	}
}

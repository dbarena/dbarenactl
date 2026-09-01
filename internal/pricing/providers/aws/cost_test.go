package aws

import (
	"math"
	"strings"
	"testing"

	"github.com/dbarena/dbarenactl/internal/pricing"
)

const epsilon = 1e-9

func approxEqual(a, b float64) bool { return math.Abs(a-b) < epsilon }

func fullItemSet() []pricing.Item {
	return []pricing.Item{
		{SKU: "COMPUTE", Unit: "Hrs", PriceUSD: 0.032, Attributes: map[string]string{"db_instance_type": "db.t4g.small"}},
		{SKU: "COMPUTE2", Unit: "Hrs", PriceUSD: 0.318, Attributes: map[string]string{"db_instance_type": "db.m6g.xlarge"}},
		{SKU: "STORAGE", Unit: "GB-Mo", PriceUSD: 0.115, Attributes: map[string]string{"disk_type": "gp3"}},
		{SKU: "STORAGE-IO2", Unit: "GB-Mo", PriceUSD: 0.125, Attributes: map[string]string{"disk_type": "io2"}},
		{SKU: "IOPS", Unit: "IOPS-Mo", PriceUSD: 0.02, Attributes: map[string]string{"disk_type": "gp3"}},
		{SKU: "IOPS-IO2", Unit: "IOPS-Mo", PriceUSD: 0.10, Attributes: map[string]string{"disk_type": "io2"}},
		{SKU: "THROUGHPUT", Unit: "MBPS-Mo", PriceUSD: 0.08},
	}
}

func TestCost_ComputeAndStorageOnly_NoOverage(t *testing.T) {
	c := Calculator{}
	bd, err := c.Cost(fullItemSet(), pricing.CostInput{
		InstanceType: "db.t4g.small", DiskGB: 20, IOPS: 3000, ThroughputMbps: 125,
	})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	wantCompute := 0.032 * pricing.HoursPerMonth
	wantStorage := 0.115 * 20
	if got := componentUSD(bd, "compute"); !approxEqual(got, wantCompute) {
		t.Errorf("compute = %v, want %v", got, wantCompute)
	}
	if got := componentUSD(bd, "storage"); !approxEqual(got, wantStorage) {
		t.Errorf("storage = %v, want %v", got, wantStorage)
	}
	if got := componentUSD(bd, "iops_overage"); got != 0 {
		t.Errorf("iops_overage = %v, want 0 (provisioned IOPS equals the default baseline)", got)
	}
	if got := componentUSD(bd, "throughput_overage"); got != 0 {
		t.Errorf("throughput_overage = %v, want 0 (provisioned throughput equals the default baseline)", got)
	}
	wantTotal := wantCompute + wantStorage
	if !approxEqual(bd.TotalUSD, wantTotal) {
		t.Errorf("TotalUSD = %v, want %v", bd.TotalUSD, wantTotal)
	}
}

// TestCost_StripedBaseline_NoOverageBilled is the regression case: a 400 GB
// volume's baseline jumps to 12,000 IOPS/500 MiB/s. Provisioning exactly
// that baseline (as a manifest's disk_baseline_iops/
// disk_baseline_throughput_mibps keys would state) must bill zero overage --
// not treat it as overage above the flat, un-striped 3,000/125 default.
func TestCost_StripedBaseline_NoOverageBilled(t *testing.T) {
	c := Calculator{}
	baselineIOPS, baselineThroughput := 12000.0, 500.0
	bd, err := c.Cost(fullItemSet(), pricing.CostInput{
		InstanceType: "db.m6g.xlarge", DiskGB: 400, IOPS: 12000, ThroughputMbps: 500,
		DiskBaselineIOPS: &baselineIOPS, DiskBaselineThroughputMbps: &baselineThroughput,
	})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if got := componentUSD(bd, "iops_overage"); got != 0 {
		t.Errorf("iops_overage = %v, want 0 -- provisioning exactly the striped baseline must not be billed as overage", got)
	}
	if got := componentUSD(bd, "throughput_overage"); got != 0 {
		t.Errorf("throughput_overage = %v, want 0", got)
	}
}

func TestCost_BaselineBoundary_OneUnitAboveAndBelow(t *testing.T) {
	c := Calculator{}
	baselineIOPS, baselineThroughput := 12000.0, 500.0

	below, err := c.Cost(fullItemSet(), pricing.CostInput{
		InstanceType: "db.m6g.xlarge", DiskGB: 400, IOPS: 11999, ThroughputMbps: 500,
		DiskBaselineIOPS: &baselineIOPS, DiskBaselineThroughputMbps: &baselineThroughput,
	})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if got := componentUSD(below, "iops_overage"); got != 0 {
		t.Errorf("1 IOPS below baseline: iops_overage = %v, want 0 (never negative)", got)
	}

	above, err := c.Cost(fullItemSet(), pricing.CostInput{
		InstanceType: "db.m6g.xlarge", DiskGB: 400, IOPS: 12001, ThroughputMbps: 501,
		DiskBaselineIOPS: &baselineIOPS, DiskBaselineThroughputMbps: &baselineThroughput,
	})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if got := componentUSD(above, "iops_overage"); !approxEqual(got, 0.02*1) {
		t.Errorf("1 IOPS above baseline: iops_overage = %v, want %v", got, 0.02*1)
	}
	if got := componentUSD(above, "throughput_overage"); !approxEqual(got, 0.08*1) {
		t.Errorf("1 MiB/s above baseline: throughput_overage = %v, want %v", got, 0.08*1)
	}
}

func TestCost_NoBaselineOverride_UsesDefaultUnstripedBaseline(t *testing.T) {
	c := Calculator{}
	bd, err := c.Cost(fullItemSet(), pricing.CostInput{
		InstanceType: "db.t4g.small", DiskGB: 20, IOPS: 3600, ThroughputMbps: 125,
	})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	// 3600 - 3000 default baseline = 600 IOPS of overage.
	if got := componentUSD(bd, "iops_overage"); !approxEqual(got, 0.02*600) {
		t.Errorf("iops_overage = %v, want %v (600 IOPS above the default 3000 baseline)", got, 0.02*600)
	}
}

func TestCost_MissingComputeSKU_Errors(t *testing.T) {
	c := Calculator{}
	_, err := c.Cost(fullItemSet(), pricing.CostInput{InstanceType: "db.r6g.does-not-exist", DiskGB: 20, IOPS: 3000, ThroughputMbps: 125})
	if err == nil {
		t.Fatal("expected an error for an instance type with no matching compute SKU")
	}
	if !strings.Contains(err.Error(), "compute rate") {
		t.Errorf("err = %v, want it to mention the missing compute rate", err)
	}
}

func TestCost_MissingStorageCategory_Errors(t *testing.T) {
	c := Calculator{}
	items := []pricing.Item{
		{SKU: "COMPUTE", Unit: "Hrs", PriceUSD: 0.032, Attributes: map[string]string{"db_instance_type": "db.t4g.small"}},
		{SKU: "IOPS", Unit: "IOPS-Mo", PriceUSD: 0.02},
		{SKU: "THROUGHPUT", Unit: "MBPS-Mo", PriceUSD: 0.08},
	}
	_, err := c.Cost(items, pricing.CostInput{InstanceType: "db.t4g.small", DiskGB: 20, IOPS: 3000, ThroughputMbps: 125})
	if err == nil {
		t.Fatal("expected an error when the storage SKU category is entirely missing from the snapshot")
	}
}

func TestCost_AmbiguousStorageSKU_Errors(t *testing.T) {
	c := Calculator{}
	items := append(fullItemSet(),
		pricing.Item{SKU: "STORAGE-DUP", Unit: "GB-Mo", PriceUSD: 0.199, Attributes: map[string]string{"disk_type": "gp3"}},
	)
	_, err := c.Cost(items, pricing.CostInput{InstanceType: "db.t4g.small", DiskType: "gp3", DiskGB: 20, IOPS: 3000, ThroughputMbps: 125})
	if err == nil {
		t.Fatal("expected an error when two storage items match the same disk type -- must not silently pick one")
	}
}

func TestCost_DiskTypeSelection_Io2(t *testing.T) {
	c := Calculator{}
	bd, err := c.Cost(fullItemSet(), pricing.CostInput{
		InstanceType: "db.t4g.small", DiskType: "io2", DiskGB: 20, IOPS: 3600, ThroughputMbps: 125,
	})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	wantStorage := 0.125 * 20 // io2 rate, not gp3's 0.115
	if got := componentUSD(bd, "storage"); !approxEqual(got, wantStorage) {
		t.Errorf("storage = %v, want %v (io2 rate)", got, wantStorage)
	}
	// 3600 - 3000 default baseline = 600 IOPS of overage, billed at io2's
	// rate (0.10), not gp3's (0.02) -- io1/io2/gp3 IOPS overage are priced
	// very differently in AWS's real Price List API, so picking the wrong
	// disk type's rate here would silently under- or over-bill.
	wantIOPS := 0.10 * 600
	if got := componentUSD(bd, "iops_overage"); !approxEqual(got, wantIOPS) {
		t.Errorf("iops_overage = %v, want %v (io2 rate)", got, wantIOPS)
	}
}

func TestCost_RequiresInstanceType(t *testing.T) {
	c := Calculator{}
	if _, err := c.Cost(fullItemSet(), pricing.CostInput{DiskGB: 20, IOPS: 3000, ThroughputMbps: 125}); err == nil {
		t.Fatal("expected an error when InstanceType is empty")
	}
}

func TestCost_RequiresPositiveDiskGB(t *testing.T) {
	c := Calculator{}
	if _, err := c.Cost(fullItemSet(), pricing.CostInput{InstanceType: "db.t4g.small", DiskGB: 0, IOPS: 3000, ThroughputMbps: 125}); err == nil {
		t.Fatal("expected an error when DiskGB is zero")
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

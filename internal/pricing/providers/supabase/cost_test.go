package supabase

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
		{SKU: "Micro", Unit: "month", PriceUSD: 10, Attributes: map[string]string{"cpu": "2-core ARM", "memory": "1 GB"}},
		{SKU: "Small", Unit: "month", PriceUSD: 15, Attributes: map[string]string{"cpu": "2-core ARM", "memory": "2 GB"}},
		{SKU: "XL", Unit: "month", PriceUSD: 210, Attributes: map[string]string{"cpu": "4-core ARM", "memory": "16 GB"}},
		{
			SKU: "plan-pro", Unit: "month", PriceUSD: 25,
			Attributes: map[string]string{"included_compute_credit_usd": "10", "included_compute_credit_covers": "Micro"},
		},
		{SKU: "disk-gp3-size", Unit: "GB-month", PriceUSD: 0.125, Attributes: map[string]string{"disk_type": "gp3", "included_amount": "8 GB"}},
		{SKU: "disk-gp3-iops", Unit: "IOPS-month", PriceUSD: 0.01, Attributes: map[string]string{"disk_type": "gp3", "included_amount": "3,000 IOPS"}},
		{SKU: "disk-gp3-throughput", Unit: "MB/s-month", PriceUSD: 0.05, Attributes: map[string]string{"disk_type": "gp3", "included_amount": "125 MB/s"}},
		{SKU: "disk-io2-size", Unit: "GB-month", PriceUSD: 0.195, Attributes: map[string]string{"disk_type": "io2"}},
		{SKU: "disk-io2-iops", Unit: "IOPS-month", PriceUSD: 0.02, Attributes: map[string]string{"disk_type": "io2"}},
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

func hasComponent(bd *pricing.CostBreakdown, name string) bool {
	for _, c := range bd.Components {
		if c.Name == name {
			return true
		}
	}
	return false
}

func TestCost_SmallProjectSize_NoCreditApplies(t *testing.T) {
	c := Calculator{}
	bd, err := c.Cost(fullItemSet(), pricing.CostInput{InstanceType: "small", DiskGB: 8, IOPS: 3000, ThroughputMbps: 125})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if got := componentUSD(bd, "pro_plan_base_fee"); !approxEqual(got, 25) {
		t.Errorf("pro_plan_base_fee = %v, want 25", got)
	}
	if got := componentUSD(bd, "compute"); !approxEqual(got, 15) {
		t.Errorf("compute = %v, want 15 (Small SKU)", got)
	}
	if hasComponent(bd, "compute_credit") {
		t.Error("credit should not apply to Small -- it only covers Micro")
	}
	// disk exactly at included amounts -- zero overage everywhere.
	if got := componentUSD(bd, "disk_size_overage"); got != 0 {
		t.Errorf("disk_size_overage = %v, want 0", got)
	}
	if got := componentUSD(bd, "disk_iops_overage"); got != 0 {
		t.Errorf("disk_iops_overage = %v, want 0", got)
	}
	if got := componentUSD(bd, "disk_throughput_overage"); got != 0 {
		t.Errorf("disk_throughput_overage = %v, want 0", got)
	}
	wantTotal := 25.0 + 15.0
	if !approxEqual(bd.TotalUSD, wantTotal) {
		t.Errorf("TotalUSD = %v, want %v", bd.TotalUSD, wantTotal)
	}
}

func TestCost_MicroProjectSize_CreditApplies(t *testing.T) {
	c := Calculator{}
	bd, err := c.Cost(fullItemSet(), pricing.CostInput{InstanceType: "micro", DiskGB: 8, IOPS: 3000, ThroughputMbps: 125})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if got := componentUSD(bd, "compute_credit"); !approxEqual(got, -10) {
		t.Errorf("compute_credit = %v, want -10", got)
	}
	wantTotal := 25.0 + 10.0 - 10.0 // pro fee + micro add-on - full credit
	if !approxEqual(bd.TotalUSD, wantTotal) {
		t.Errorf("TotalUSD = %v, want %v", bd.TotalUSD, wantTotal)
	}
}

func TestCost_ProjectSizeVocabularyTranslation(t *testing.T) {
	c := Calculator{}
	// "xlarge" (manifest vocabulary) must resolve to the "XL" SKU
	// (pricing.md vocabulary) -- not a plain case-fold of the input.
	bd, err := c.Cost(fullItemSet(), pricing.CostInput{InstanceType: "xlarge", DiskGB: 8, IOPS: 3000, ThroughputMbps: 125})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if got := componentUSD(bd, "compute"); !approxEqual(got, 210) {
		t.Errorf("compute = %v, want 210 (xlarge -> XL SKU)", got)
	}
}

func TestCost_UnrecognizedProjectSize_Errors(t *testing.T) {
	c := Calculator{}
	if _, err := c.Cost(fullItemSet(), pricing.CostInput{InstanceType: "not-a-real-size", DiskGB: 8}); err == nil {
		t.Fatal("expected an error for an unrecognized project_size")
	}
}

func TestCost_DiskOverage_Gp3(t *testing.T) {
	c := Calculator{}
	bd, err := c.Cost(fullItemSet(), pricing.CostInput{InstanceType: "small", DiskGB: 28, IOPS: 4000, ThroughputMbps: 200})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	// 28 - 8 included = 20 GB overage @ $0.125/GB-mo
	if got := componentUSD(bd, "disk_size_overage"); !approxEqual(got, 0.125*20) {
		t.Errorf("disk_size_overage = %v, want %v", got, 0.125*20)
	}
	// 4000 - 3000 included = 1000 IOPS overage @ $0.01/IOPS-mo
	if got := componentUSD(bd, "disk_iops_overage"); !approxEqual(got, 0.01*1000) {
		t.Errorf("disk_iops_overage = %v, want %v", got, 0.01*1000)
	}
	// 200 - 125 included = 75 MB/s overage @ $0.05/MBps-mo
	if got := componentUSD(bd, "disk_throughput_overage"); !approxEqual(got, 0.05*75) {
		t.Errorf("disk_throughput_overage = %v, want %v", got, 0.05*75)
	}
}

func TestCost_DiskOverage_CommaFormattedIncludedAmount(t *testing.T) {
	// disk-gp3-iops's included_amount is "3,000 IOPS" -- must parse past
	// the comma, not choke on it or silently treat it as included=3 (or
	// included=0, over-billing every test point).
	c := Calculator{}
	bd, err := c.Cost(fullItemSet(), pricing.CostInput{InstanceType: "small", DiskGB: 8, IOPS: 3000, ThroughputMbps: 125})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if got := componentUSD(bd, "disk_iops_overage"); got != 0 {
		t.Errorf("disk_iops_overage = %v, want 0 (3000 IOPS requested == 3,000 IOPS included)", got)
	}
}

func TestCost_Io2_NoIncludedAmount_BillsInFull(t *testing.T) {
	c := Calculator{}
	bd, err := c.Cost(fullItemSet(), pricing.CostInput{InstanceType: "small", DiskType: "io2", DiskGB: 20, IOPS: 5000, ThroughputMbps: 300})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if got := componentUSD(bd, "disk_size_overage"); !approxEqual(got, 0.195*20) {
		t.Errorf("disk_size_overage = %v, want %v (io2 has no included amount)", got, 0.195*20)
	}
	if got := componentUSD(bd, "disk_iops_overage"); !approxEqual(got, 0.02*5000) {
		t.Errorf("disk_iops_overage = %v, want %v (io2 has no included amount)", got, 0.02*5000)
	}
	if got := componentUSD(bd, "disk_throughput_overage"); got != 0 {
		t.Errorf("disk_throughput_overage = %v, want 0 -- io2 throughput scales automatically with IOPS and is never separately billed", got)
	}
}

func TestCost_MissingComputeSKU_Errors(t *testing.T) {
	c := Calculator{}
	var items []pricing.Item
	for _, it := range fullItemSet() {
		if it.SKU != "Small" {
			items = append(items, it)
		}
	}
	_, err := c.Cost(items, pricing.CostInput{InstanceType: "small", DiskGB: 8})
	if err == nil {
		t.Fatal("expected an error when the compute SKU for this project_size is missing from the snapshot")
	}
	if !strings.Contains(err.Error(), "compute add-on rate") {
		t.Errorf("err = %v, want it to mention the missing compute add-on rate", err)
	}
}

func TestVCPUAndRAMGB(t *testing.T) {
	vcpu, ramGB, err := VCPUAndRAMGB(fullItemSet(), "xlarge")
	if err != nil {
		t.Fatalf("VCPUAndRAMGB: %v", err)
	}
	if vcpu != 4 || ramGB != 16 {
		t.Errorf("vcpu=%v ramGB=%v, want 4, 16", vcpu, ramGB)
	}
}

func TestVCPUAndRAMGB_UnrecognizedProjectSize_Errors(t *testing.T) {
	if _, _, err := VCPUAndRAMGB(fullItemSet(), "not-a-real-size"); err == nil {
		t.Fatal("expected an error for an unrecognized project_size")
	}
}

// itemSetWithThroughputRate is fullItemSet with disk-gp3-throughput's rate
// swapped to 0.095 -- reproduces the exact rate that produced the reported
// amount_usd/detail mismatch bug (0.095 * 125 overage MB/s = 11.875, a value
// with real information at the third decimal place).
func itemSetWithThroughputRate(rate float64) []pricing.Item {
	items := fullItemSet()
	for i := range items {
		if items[i].SKU == "disk-gp3-throughput" {
			items[i].PriceUSD = rate
		}
	}
	return items
}

// TestCost_DiskThroughputOverage_AmountAndDetailAgree is the regression test
// for the reported bug: amount_usd=11.875 but detail said "...= $11.88"
// (Detail's %.2f threw away real sub-cent precision AmountUSD retained).
// Both must now be derived from the same MoneyDecimals-rounded value.
func TestCost_DiskThroughputOverage_AmountAndDetailAgree(t *testing.T) {
	cases := []struct {
		name           string
		throughputMbps float64
		wantAmountUSD  float64
		wantDetailTail string
	}{
		{"overage_125_at_rate_0.095", 250, 11.875, "$11.875"}, // 250-125 included = 125 overage
		{"overage_375_at_rate_0.095", 500, 35.625, "$35.625"}, // 500-125 included = 375 overage
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Calculator{}
			bd, err := c.Cost(itemSetWithThroughputRate(0.095), pricing.CostInput{
				InstanceType: "small", DiskGB: 8, IOPS: 3000, ThroughputMbps: tc.throughputMbps,
			})
			if err != nil {
				t.Fatalf("Cost: %v", err)
			}
			var detail string
			var amount float64
			for _, comp := range bd.Components {
				if comp.Name == "disk_throughput_overage" {
					detail, amount = comp.Detail, comp.AmountUSD
				}
			}
			if amount != tc.wantAmountUSD {
				t.Errorf("disk_throughput_overage.AmountUSD = %v, want %v", amount, tc.wantAmountUSD)
			}
			if !strings.HasSuffix(detail, tc.wantDetailTail) {
				t.Errorf("disk_throughput_overage.Detail = %q, want it to end with %q", detail, tc.wantDetailTail)
			}
		})
	}
}

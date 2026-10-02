package supabase

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/dbarena/dbarenactl/internal/pricing"
)

// skuForProjectSize translates dbarenactl's candidate manifest project_size
// vocabulary ("small", "xlarge", "2xlarge", ...) into the exact SKU string
// supabase.com/pricing.md's Compute Add-Ons table uses ("Small", "XL",
// "2XL", ...) -- these differ in more than just case, so this mapping is
// maintained explicitly rather than derived.
var skuForProjectSize = map[string]string{
	"micro": "Micro", "small": "Small", "medium": "Medium", "large": "Large",
	"xlarge": "XL", "2xlarge": "2XL", "4xlarge": "4XL", "8xlarge": "8XL",
	"12xlarge": "12XL", "16xlarge": "16XL",
}

func skuFor(projectSize string) (string, error) {
	sku, ok := skuForProjectSize[strings.ToLower(projectSize)]
	if !ok {
		return "", fmt.Errorf("supabase: cost: unrecognized project_size %q -- no known compute SKU mapping for it", projectSize)
	}
	return sku, nil
}

// Calculator computes a monthly cost estimate for one Supabase project
// configuration from a pricing.Snapshot's Items (as produced by this
// package's own Fetcher, parsed live from supabase.com/pricing.md).
type Calculator struct{}

// Cost implements pricing.Calculator. in.InstanceType is the candidate
// manifest's project_size (e.g. "small", "xlarge"); in.DiskType is the
// candidate manifest's disk_type (e.g. "gp3", "io2") -- every candidate
// declares it explicitly, so there's no default to fall back to.
func (Calculator) Cost(items []pricing.Item, in pricing.CostInput) (*pricing.CostBreakdown, error) {
	if in.InstanceType == "" {
		return nil, fmt.Errorf("supabase: cost: instance type (project_size) is required")
	}
	if in.DiskType == "" {
		return nil, fmt.Errorf("supabase: cost: disk type is required")
	}
	sku, err := skuFor(in.InstanceType)
	if err != nil {
		return nil, err
	}
	diskType := in.DiskType

	computeItem, err := findOneItem(items, func(it pricing.Item) bool {
		return it.SKU == sku && it.Unit == "month"
	}, fmt.Sprintf("compute add-on rate for project_size %q (SKU %q)", in.InstanceType, sku))
	if err != nil {
		return nil, err
	}
	proItem, err := findOneItem(items, func(it pricing.Item) bool {
		return it.SKU == "plan-pro"
	}, "Pro plan base fee")
	if err != nil {
		return nil, err
	}

	components := []pricing.CostComponent{
		{
			Name:      "pro_plan_base_fee",
			AmountUSD: pricing.RoundTo(proItem.PriceUSD, pricing.MoneyDecimals),
			Detail:    fmt.Sprintf("sku=%s $%.*f/month", proItem.SKU, pricing.MoneyDecimals, pricing.RoundTo(proItem.PriceUSD, pricing.MoneyDecimals)),
		},
		{
			Name:      "compute",
			AmountUSD: pricing.RoundTo(computeItem.PriceUSD, pricing.MoneyDecimals),
			Detail: fmt.Sprintf("sku=%s project_size=%s $%.*f/month", computeItem.SKU, in.InstanceType,
				pricing.MoneyDecimals, pricing.RoundTo(computeItem.PriceUSD, pricing.MoneyDecimals)),
		},
	}

	// The Pro/Team compute credit covers exactly one instance of the size
	// named in included_compute_credit_covers -- it does not partially
	// offset any other size. Applying it as a plain credit line (rather
	// than netting it against the compute add-on) keeps every component
	// independently auditable.
	creditUSD := 0.0
	if covers, ok := proItem.Attributes["included_compute_credit_covers"]; ok && strings.EqualFold(covers, in.InstanceType) {
		amt, err := strconv.ParseFloat(proItem.Attributes["included_compute_credit_usd"], 64)
		if err != nil {
			return nil, fmt.Errorf("supabase: cost: parse included_compute_credit_usd %q: %w", proItem.Attributes["included_compute_credit_usd"], err)
		}
		creditUSD = pricing.RoundTo(amt, pricing.MoneyDecimals)
		components = append(components, pricing.CostComponent{
			Name:      "compute_credit",
			AmountUSD: -creditUSD,
			Detail:    fmt.Sprintf("Pro plan credit covers one %s instance: -$%.*f/month", in.InstanceType, pricing.MoneyDecimals, creditUSD),
		})
	}

	diskComps, err := diskComponents(items, diskType, in)
	if err != nil {
		return nil, err
	}
	components = append(components, diskComps...)

	return &pricing.CostBreakdown{Components: components, TotalUSD: pricing.SumComponents(components)}, nil
}

// diskComponents prices disk size/IOPS/throughput overage above whatever
// each disk item's own included_amount states (gp3 only -- io2 items carry
// no included_amount, meaning every provisioned unit is billed, and io2 has
// no separate throughput item at all: pricing.md states its throughput
// "scales automatically with IOPS", i.e. it is never billed on its own).
func diskComponents(items []pricing.Item, diskType string, in pricing.CostInput) ([]pricing.CostComponent, error) {
	sizeItem, err := findOneItem(items, func(it pricing.Item) bool { return it.SKU == "disk-"+diskType+"-size" },
		fmt.Sprintf("disk size rate for disk_type %q", diskType))
	if err != nil {
		return nil, err
	}
	iopsItem, err := findOneItem(items, func(it pricing.Item) bool { return it.SKU == "disk-"+diskType+"-iops" },
		fmt.Sprintf("disk IOPS rate for disk_type %q", diskType))
	if err != nil {
		return nil, err
	}

	sizeIncluded, err := includedAmount(sizeItem)
	if err != nil {
		return nil, err
	}
	iopsIncluded, err := includedAmount(iopsItem)
	if err != nil {
		return nil, err
	}
	sizeOverage := maxFloat(0, in.DiskGB-sizeIncluded)
	iopsOverage := maxFloat(0, in.IOPS-iopsIncluded)
	sizeUSD := pricing.RoundTo(sizeItem.PriceUSD*sizeOverage, pricing.MoneyDecimals)
	iopsUSD := pricing.RoundTo(iopsItem.PriceUSD*iopsOverage, pricing.MoneyDecimals)

	components := []pricing.CostComponent{
		{
			Name:      "disk_size_overage",
			AmountUSD: sizeUSD,
			Detail: fmt.Sprintf("sku=%s included=%g GB provisioned=%g overage=%g rate=$%.6f/GB-mo = $%.*f",
				sizeItem.SKU, sizeIncluded, in.DiskGB, sizeOverage, sizeItem.PriceUSD, pricing.MoneyDecimals, sizeUSD),
		},
		{
			Name:      "disk_iops_overage",
			AmountUSD: iopsUSD,
			Detail: fmt.Sprintf("sku=%s included=%g IOPS provisioned=%g overage=%g rate=$%.6f/IOPS-mo = $%.*f",
				iopsItem.SKU, iopsIncluded, in.IOPS, iopsOverage, iopsItem.PriceUSD, pricing.MoneyDecimals, iopsUSD),
		},
	}

	if diskType != "gp3" {
		// io2's throughput "scales automatically with IOPS" per
		// pricing.md -- never billed as its own line item.
		components = append(components, pricing.CostComponent{Name: "disk_throughput_overage", AmountUSD: 0, Detail: "io2 throughput scales automatically with IOPS -- not separately billed"})
		return components, nil
	}
	throughputItem, err := findOneItem(items, func(it pricing.Item) bool { return it.SKU == "disk-gp3-throughput" }, "gp3 disk throughput rate")
	if err != nil {
		return nil, err
	}
	throughputIncluded, err := includedAmount(throughputItem)
	if err != nil {
		return nil, err
	}
	throughputOverage := maxFloat(0, in.ThroughputMbps-throughputIncluded)
	throughputUSD := pricing.RoundTo(throughputItem.PriceUSD*throughputOverage, pricing.MoneyDecimals)
	components = append(components, pricing.CostComponent{
		Name:      "disk_throughput_overage",
		AmountUSD: throughputUSD,
		Detail: fmt.Sprintf("sku=%s included=%g MB/s provisioned=%g overage=%g rate=$%.6f/MBps-mo = $%.*f",
			throughputItem.SKU, throughputIncluded, in.ThroughputMbps, throughputOverage, throughputItem.PriceUSD, pricing.MoneyDecimals, throughputUSD),
	})
	return components, nil
}

var includedAmountRe = regexp.MustCompile(`^([\d,]+(?:\.\d+)?)`)

// includedAmount parses the leading number out of an item's included_amount
// attribute (e.g. "20 GB" -> 20, "12,000 IOPS" -> 12000). Absent entirely
// (as for io2, which has none) means nothing is included for free: 0.
func includedAmount(it pricing.Item) (float64, error) {
	raw, ok := it.Attributes["included_amount"]
	if !ok {
		return 0, nil
	}
	m := includedAmountRe.FindStringSubmatch(raw)
	if m == nil {
		return 0, fmt.Errorf("supabase: cost: could not parse included_amount %q on sku %s", raw, it.SKU)
	}
	return strconv.ParseFloat(strings.ReplaceAll(m[1], ",", ""), 64)
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// findOneItem returns the single item in items matching match, or an error
// naming what was being looked for if zero or more than one matched.
func findOneItem(items []pricing.Item, match func(pricing.Item) bool, what string) (pricing.Item, error) {
	var found []pricing.Item
	for _, it := range items {
		if match(it) {
			found = append(found, it)
		}
	}
	switch len(found) {
	case 0:
		return pricing.Item{}, fmt.Errorf("supabase: no pricing item found for %s -- run `dbarenactl pricing fetch` again, or check the snapshot with `dbarenactl pricing show supabase`", what)
	case 1:
		return found[0], nil
	default:
		return pricing.Item{}, fmt.Errorf("supabase: %d pricing items matched %s, expected exactly one: %+v", len(found), what, found)
	}
}

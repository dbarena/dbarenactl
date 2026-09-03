package aws

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/dbarena/dbarenactl/internal/pricing"
)

// StorageDocsURL is AWS's own current documentation for gp3 baseline
// storage performance -- cited wherever this file assumes a baseline IOPS/
// throughput level not present in the Price List API's data (the API gives
// rates, not which amount of performance is included free).
const StorageDocsURL = "https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/CHAP_Storage.html"

// defaultBaselineIOPS and defaultBaselineThroughputMbps are gp3's documented
// unconditional baseline for any RDS Postgres volume (per StorageDocsURL):
// "Amazon RDS provides a baseline storage performance of 3000 IOPS and
// 125 MiB/s" -- true regardless of volume size, up to the point a volume
// crosses the striping threshold and gets a higher baseline. That higher
// baseline is never hardcoded here: a candidate manifest that provisions a
// volume above the threshold states its own baseline explicitly via the
// disk_baseline_iops/disk_baseline_throughput_mibps pricing: keys (see
// CostInput.DiskBaselineIOPS/DiskBaselineThroughputMbps), authored by
// whoever decided to cross it, citing this same URL.
const (
	defaultBaselineIOPS           float64 = 3000
	defaultBaselineThroughputMbps float64 = 125
	defaultDiskType                       = "gp3"
)

// Calculator computes a monthly on-demand cost estimate for one RDS
// instance configuration from a pricing.Snapshot's Items.
type Calculator struct{}

// Cost implements pricing.Calculator. It never guesses: if the snapshot is
// missing any of the four expected SKU categories (compute/storage/
// IOPS-overage/throughput-overage) for the given configuration, or a
// required CostInput field is unset, it returns an error instead of a
// partial or wrong number.
func (Calculator) Cost(items []pricing.Item, in pricing.CostInput) (*pricing.CostBreakdown, error) {
	if in.InstanceType == "" {
		return nil, fmt.Errorf("aws: cost: instance type is required")
	}
	if in.DiskGB <= 0 {
		return nil, fmt.Errorf("aws: cost: disk_gb must be positive, got %g", in.DiskGB)
	}
	diskType := in.DiskType
	if diskType == "" {
		diskType = defaultDiskType
	}

	computeItem, err := findOneItem(items, func(it pricing.Item) bool {
		return unitIs(it.Unit, "hrs") && it.Attributes["db_instance_type"] == in.InstanceType
	}, fmt.Sprintf("compute rate for instance type %q", in.InstanceType))
	if err != nil {
		return nil, err
	}
	storageItem, err := findOneItem(items, func(it pricing.Item) bool {
		return unitIs(it.Unit, "gb-mo") && matchesDiskType(it, diskType)
	}, fmt.Sprintf("storage rate for disk type %q", diskType))
	if err != nil {
		return nil, err
	}
	iopsItem, err := findOneItem(items, func(it pricing.Item) bool {
		return strings.Contains(strings.ToUpper(it.Unit), "IOPS") && matchesDiskType(it, diskType)
	}, fmt.Sprintf("IOPS overage rate for disk type %q", diskType))
	if err != nil {
		return nil, err
	}
	throughputItem, err := findOneItem(items, func(it pricing.Item) bool {
		u := strings.ToUpper(it.Unit)
		return strings.Contains(u, "BPS") && !strings.Contains(u, "IOPS")
	}, "throughput overage rate")
	if err != nil {
		return nil, err
	}

	baselineIOPS, baselineIOPSSource := defaultBaselineIOPS, fmt.Sprintf("default gp3 baseline per %s", StorageDocsURL)
	if in.DiskBaselineIOPS != nil {
		baselineIOPS, baselineIOPSSource = *in.DiskBaselineIOPS, "pricing.disk_baseline_iops"
	}
	baselineThroughput, baselineThroughputSource := float64(defaultBaselineThroughputMbps), fmt.Sprintf("default gp3 baseline per %s", StorageDocsURL)
	if in.DiskBaselineThroughputMbps != nil {
		baselineThroughput, baselineThroughputSource = *in.DiskBaselineThroughputMbps, "pricing.disk_baseline_throughput_mibps"
	}

	computeUSD := computeItem.PriceUSD * pricing.HoursPerMonth
	storageUSD := storageItem.PriceUSD * in.DiskGB
	iopsOverage := maxFloat(0, in.IOPS-baselineIOPS)
	iopsUSD := iopsItem.PriceUSD * iopsOverage
	throughputOverage := maxFloat(0, in.ThroughputMbps-baselineThroughput)
	throughputUSD := throughputItem.PriceUSD * throughputOverage

	breakdown := &pricing.CostBreakdown{
		Components: []pricing.CostComponent{
			{
				Name:      "compute",
				AmountUSD: computeUSD,
				Detail: fmt.Sprintf("sku=%s instance_type=%s rate=$%.6f/hr x %d hr = $%.2f",
					computeItem.SKU, in.InstanceType, computeItem.PriceUSD, pricing.HoursPerMonth, computeUSD),
			},
			{
				Name:      "storage",
				AmountUSD: storageUSD,
				Detail: fmt.Sprintf("sku=%s disk_type=%s rate=$%.6f/GB-mo x %g GB = $%.2f",
					storageItem.SKU, diskType, storageItem.PriceUSD, in.DiskGB, storageUSD),
			},
			{
				Name:      "iops_overage",
				AmountUSD: iopsUSD,
				Detail: fmt.Sprintf("sku=%s baseline=%g (%s) provisioned=%g overage=%g rate=$%.6f/IOPS-mo = $%.2f",
					iopsItem.SKU, baselineIOPS, baselineIOPSSource, in.IOPS, iopsOverage, iopsItem.PriceUSD, iopsUSD),
			},
			{
				Name:      "throughput_overage",
				AmountUSD: throughputUSD,
				Detail: fmt.Sprintf("sku=%s baseline=%g MiB/s (%s) provisioned=%g overage=%g rate=$%.6f/MiBps-mo = $%.2f",
					throughputItem.SKU, baselineThroughput, baselineThroughputSource, in.ThroughputMbps, throughputOverage, throughputItem.PriceUSD, throughputUSD),
			},
		},
		TotalUSD: computeUSD + storageUSD + iopsUSD + throughputUSD,
	}
	return breakdown, nil
}

// VCPUAndRAMGB reads vcpu/RAM for instanceType from the matched compute
// item's own Attributes (as returned by AWS's Price List API), rather than
// hardcoding a lookup table that could drift from what AWS actually
// provisions for that instance class.
func VCPUAndRAMGB(items []pricing.Item, instanceType string) (vcpu, ramGB float64, err error) {
	item, err := findOneItem(items, func(it pricing.Item) bool {
		return unitIs(it.Unit, "hrs") && it.Attributes["db_instance_type"] == instanceType
	}, fmt.Sprintf("compute rate for instance type %q", instanceType))
	if err != nil {
		return 0, 0, err
	}
	vcpuStr, ok := item.Attributes["vcpu"]
	if !ok {
		return 0, 0, fmt.Errorf("aws: instance type %q pricing item has no vcpu attribute", instanceType)
	}
	vcpu, err = strconv.ParseFloat(vcpuStr, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("aws: parse vcpu %q: %w", vcpuStr, err)
	}
	memStr, ok := item.Attributes["memory"]
	if !ok {
		return 0, 0, fmt.Errorf("aws: instance type %q pricing item has no memory attribute", instanceType)
	}
	ramGB, err = parseLeadingNumber(memStr)
	if err != nil {
		return 0, 0, fmt.Errorf("aws: parse memory %q: %w", memStr, err)
	}
	return vcpu, ramGB, nil
}

var leadingNumberRe = regexp.MustCompile(`^([\d.]+)`)

func parseLeadingNumber(s string) (float64, error) {
	m := leadingNumberRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("no leading number found in %q", s)
	}
	return strconv.ParseFloat(m[1], 64)
}

// matchesDiskType reports whether it's a storage item for diskType. Storage
// items always carry a disk_type attribute (normalized from AWS's raw
// volumeType by normalizeAttributes) -- an item missing that attribute is
// never a storage rate and never matches.
func matchesDiskType(it pricing.Item, diskType string) bool {
	dt, ok := it.Attributes["disk_type"]
	return ok && dt == diskType
}

func unitIs(unit, want string) bool { return strings.EqualFold(unit, want) }

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// findOneItem returns the single item in items matching match, or an error
// naming what was being looked for if zero or more than one matched --
// ambiguity is treated the same as absence: both mean the caller cannot
// safely price this component.
func findOneItem(items []pricing.Item, match func(pricing.Item) bool, what string) (pricing.Item, error) {
	var found []pricing.Item
	for _, it := range items {
		if match(it) {
			found = append(found, it)
		}
	}
	switch len(found) {
	case 0:
		return pricing.Item{}, fmt.Errorf("aws: no pricing item found for %s -- run `dbarenactl pricing fetch` again, or check the snapshot with `dbarenactl pricing show aws`", what)
	case 1:
		return found[0], nil
	default:
		return pricing.Item{}, fmt.Errorf("aws: %d pricing items matched %s, expected exactly one: %+v", len(found), what, found)
	}
}

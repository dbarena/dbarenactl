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

// BurstDocsURL and UnlimitedModeDocsURL are AWS's own current documentation
// for burstable (T-family) CPU credits and RDS's Unlimited-mode default,
// respectively -- cited in cpuCreditOverageUSD, the only place this file
// bills a cost not present as a flat rate in the Price List API's data (the
// API gives the credit-overage *rate*, not the baseline utilization it's
// charged above).
const (
	BurstDocsURL         = "https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/burstable-credits-baseline-concepts.html"
	UnlimitedModeDocsURL = "https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/Concepts.DBInstanceClass.Types.html"
)

// burstBaseline is one row of BurstDocsURL's per-instance-size credit
// table: how many vCPUs the instance has, and the fraction of each vCPU it
// can use for a net credit balance of zero (its "baseline utilization")
// before it starts drawing down, and once exhausted paying for, CPU
// credits.
type burstBaseline struct {
	BaselinePercent float64 // e.g. 0.20 = 20% per vCPU
}

// burstBaselines lists only the burstable instance types actually used by
// a candidate manifest today (candidates/aws/rds/tpcc/manifest.yaml's
// small/medium tiers). A burstable instance type requested but not listed
// here is a hard error in cpuCreditOverageUSD -- never a guess -- so add
// its row from BurstDocsURL's table before pricing it.
var burstBaselines = map[string]burstBaseline{
	"db.t4g.small":  {BaselinePercent: 0.20},
	"db.t4g.medium": {BaselinePercent: 0.20},
}

// burstFamilyRe extracts a burstable instance type's family token (e.g.
// "T4G" from "db.t4g.small") -- the key the Price List API's "CPU Credits"
// product family attaches its per-vCPU-hour overage rate to, as
// instance_family. Matches AWS's T-class burstable naming: t2, t3, t3a,
// t4g, t8i, and future sizes/generations of the same shape.
var burstFamilyRe = regexp.MustCompile(`^db\.(t[0-9][a-z]*)\.`)

// cpuCreditOverageUSD bills the RDS Unlimited-mode CPU-credit surcharge for
// a burstable (T-family) instanceType. Returns usd=0, detail="", err=nil
// for any non-burstable instanceType (e.g. "db.m9g.large").
//
// Example to explain why this surcharge, in practice, covers nearly all of
// dbarenactl's benchmark measurement window for db.t4g.small/medium
// (2 vCPUs, 20% baseline utilization per vCPU, per BurstDocsURL's table):
//
//	earn rate                = 0.20 baseline * 2 vCPU * 60 min/hr = 24 credits/hr = 0.4 credits/min
//	spend rate (at 100% CPU) = 1.00 utilization * 2 vCPU * 1 min  = 2 credits/min
//	net drain while saturated = 2 - 0.4                           = 1.6 credits/min
//
// T-family instances earn no "launch credits" (a T2-only concept, per
// BurstDocsURL) and benchctl provisions each RDS instance fresh per run, so
// the accrued balance starts at ~0. Even a generous 2-hour pre-benchmark
// "prepare" step (schema/data load) idling near 0% CPU accrues only
// ~48 credits (2 hr * 24 credits/hr) only enough to cover roughly
// 48 / 1.6 ~= 30 minutes of saturation. The warmup and measured steps
// both run the *same* target concurrency, so CPU  saturation starts
// almost immediately once warmup begins. That leaves, aside from perhaps
// its first few minutes, the entire warmup+benchmark window running on
// paid "surplus credits" -- i.e. billed as overage, per UnlimitedModeDocsURL.
//
// pricing.HoursPerMonth already extrapolates the flat compute rate as "this
// configuration runs continuously all month"; this function extrapolates
// the same observed saturation the same way, billing the excess-over-
// baseline vCPU-hours for all 730 hours rather than netting out a credit
// reserve a freshly launched instance doesn't have.
func cpuCreditOverageUSD(items []pricing.Item, instanceType string, computeItem pricing.Item) (usd float64, detail string, err error) {
	m := burstFamilyRe.FindStringSubmatch(instanceType)
	if m == nil {
		return 0, "", nil // not a burstable instance type
	}
	family := strings.ToUpper(m[1])

	baseline, ok := burstBaselines[instanceType]
	if !ok {
		return 0, "", fmt.Errorf("aws: %q is a burstable instance type with no documented CPU-credit baseline. "+
			"Add its baseline utilization from %s to burstBaselines before pricing it", instanceType, BurstDocsURL)
	}

	vcpuStr, ok := computeItem.Attributes["vcpu"]
	if !ok {
		return 0, "", fmt.Errorf("aws: instance type %q pricing item has no vcpu attribute", instanceType)
	}
	vcpu, err := strconv.ParseFloat(vcpuStr, 64)
	if err != nil {
		return 0, "", fmt.Errorf("aws: parse vcpu %q: %w", vcpuStr, err)
	}

	creditItem, err := findOneItem(items, func(it pricing.Item) bool {
		return unitIs(it.Unit, "vCPU-Hours") && it.Attributes["instance_family"] == family
	}, fmt.Sprintf("CPU credit overage rate for instance family %q", family))
	if err != nil {
		return 0, "", err
	}

	overageVCPUHoursPerHour := vcpu * (1 - baseline.BaselinePercent)
	usd = pricing.RoundTo(creditItem.PriceUSD*overageVCPUHoursPerHour*pricing.HoursPerMonth, pricing.MoneyDecimals)
	detail = fmt.Sprintf("sku=%s instance_family=%s vcpu=%g baseline=%.0f%% (assumes sustained saturation exhausts credits, per %s and %s) overage=%g vcpu-hr/hr rate=$%.6f/vcpu-hr x %d hr = $%.*f",
		creditItem.SKU, family, vcpu, baseline.BaselinePercent*100, BurstDocsURL, UnlimitedModeDocsURL,
		overageVCPUHoursPerHour, creditItem.PriceUSD, pricing.HoursPerMonth, pricing.MoneyDecimals, usd)
	return usd, detail, nil
}

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

	computeUSD := pricing.RoundTo(computeItem.PriceUSD*pricing.HoursPerMonth, pricing.MoneyDecimals)
	storageUSD := pricing.RoundTo(storageItem.PriceUSD*in.DiskGB, pricing.MoneyDecimals)
	iopsOverage := maxFloat(0, in.IOPS-baselineIOPS)
	iopsUSD := pricing.RoundTo(iopsItem.PriceUSD*iopsOverage, pricing.MoneyDecimals)
	throughputOverage := maxFloat(0, in.ThroughputMbps-baselineThroughput)
	throughputUSD := pricing.RoundTo(throughputItem.PriceUSD*throughputOverage, pricing.MoneyDecimals)

	components := []pricing.CostComponent{
		{
			Name:      "compute",
			AmountUSD: computeUSD,
			Detail: fmt.Sprintf("sku=%s instance_type=%s rate=$%.6f/hr x %d hr = $%.*f",
				computeItem.SKU, in.InstanceType, computeItem.PriceUSD, pricing.HoursPerMonth, pricing.MoneyDecimals, computeUSD),
		},
		{
			Name:      "storage",
			AmountUSD: storageUSD,
			Detail: fmt.Sprintf("sku=%s disk_type=%s rate=$%.6f/GB-mo x %g GB = $%.*f",
				storageItem.SKU, diskType, storageItem.PriceUSD, in.DiskGB, pricing.MoneyDecimals, storageUSD),
		},
		{
			Name:      "iops_overage",
			AmountUSD: iopsUSD,
			Detail: fmt.Sprintf("sku=%s baseline=%g (%s) provisioned=%g overage=%g rate=$%.6f/IOPS-mo = $%.*f",
				iopsItem.SKU, baselineIOPS, baselineIOPSSource, in.IOPS, iopsOverage, iopsItem.PriceUSD, pricing.MoneyDecimals, iopsUSD),
		},
		{
			Name:      "throughput_overage",
			AmountUSD: throughputUSD,
			Detail: fmt.Sprintf("sku=%s baseline=%g MiB/s (%s) provisioned=%g overage=%g rate=$%.6f/MiBps-mo = $%.*f",
				throughputItem.SKU, baselineThroughput, baselineThroughputSource, in.ThroughputMbps, throughputOverage, throughputItem.PriceUSD, pricing.MoneyDecimals, throughputUSD),
		},
	}

	creditUSD, creditDetail, err := cpuCreditOverageUSD(items, in.InstanceType, computeItem)
	if err != nil {
		return nil, err
	}
	if creditDetail != "" {
		components = append(components, pricing.CostComponent{
			Name:      "cpu_credit_overage",
			AmountUSD: creditUSD,
			Detail:    creditDetail,
		})
	}

	breakdown := &pricing.CostBreakdown{
		Components: components,
		TotalUSD:   pricing.SumComponents(components),
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

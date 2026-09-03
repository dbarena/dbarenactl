package gcp

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/dbarena/dbarenactl/internal/pricing"
)

// tierCustomRe matches Cloud SQL Enterprise's "db-custom-<family>-<vcpu>-<ram_mb>"
// shape, e.g. "db-custom-N4-2-4096" (2 vCPU, 4096 MiB RAM). The family
// (e.g. "N4") is captured, not discarded: the Cloud Billing Catalog prices
// several machine families/editions concurrently in the same region, so the
// family is needed later to pick the right SKU (see matchesFamily).
var tierCustomRe = regexp.MustCompile(`^db-custom-([A-Za-z0-9]+)-(\d+)-(\d+)$`)

// tierPerfOptimizedRe matches Cloud SQL Enterprise Plus's
// "db-perf-optimized-<family>-<vcpu>" shape, e.g. "db-perf-optimized-N-2".
// This series has a fixed 1 vCPU : 8 GB RAM ratio (documented in
// candidates/gcp-cloudsql-enterprise-plus-tpcc.yaml's header comment) --
// there is no separate RAM component in the tier string to parse. The family
// segment requires at least one character (not "*"): an empty family would
// otherwise later build a "\b\b" word-boundary regex in matchesFamily that
// matches every description, defeating the disambiguation it exists for.
var tierPerfOptimizedRe = regexp.MustCompile(`^db-perf-optimized-([A-Za-z0-9]+)-(\d+)$`)

const perfOptimizedRAMPerVCPUGB = 8

// HyperdiskDocsURL is Google's own current documentation for Hyperdisk
// Balanced (the disk type Cloud SQL's N4-based tiers provision): capacity,
// provisioned IOPS, and provisioned throughput are each billed for their
// full amount, with no included/free baseline the way AWS gp3 has -- so
// defaultBaselineIOPS/defaultBaselineThroughputMbps are 0, not a nonzero
// floor.
const HyperdiskDocsURL = "https://cloud.google.com/compute/docs/disks/hyperdisks"

const (
	defaultBaselineIOPS           float64 = 0
	defaultBaselineThroughputMbps float64 = 0
)

// VCPUAndRAMGB parses a Cloud SQL tier string into (vcpu, ram_gb). items is
// unused -- present only so this matches the signature of the AWS/Supabase
// equivalents (which do need their snapshot's items), letting callers treat
// all three providers uniformly. It never guesses: an unrecognized shape is
// an error, not a silent zero.
func VCPUAndRAMGB(_ []pricing.Item, tier string) (vcpu, ramGB float64, err error) {
	pt, err := parseTier(tier)
	if err != nil {
		return 0, 0, err
	}
	return pt.VCPU, pt.RAMGB, nil
}

func vcpuAndRAMGB(tier string) (vcpu, ramGB float64, err error) {
	return VCPUAndRAMGB(nil, tier)
}

// edition identifies which Cloud SQL edition a tier belongs to. GCP prices
// Enterprise and Enterprise Plus SKUs concurrently in the same region under
// overlapping keywords (both have "vCPU"/"RAM"/"storage" in their
// descriptions), so edition is one of the two axes (with family) needed to
// pick the right SKU out of the Cloud Billing Catalog.
type edition string

const (
	editionEnterprise     edition = "Enterprise"
	editionEnterprisePlus edition = "Enterprise Plus"
)

// parsedTier is a tier string broken into the facts needed to price it:
// vcpu/ram_gb for the compute formula, and edition/family to pick the right
// SKU out of a snapshot that prices multiple editions/families at once.
type parsedTier struct {
	VCPU, RAMGB float64
	Edition     edition
	Family      string
}

// parseTier never guesses: an unrecognized shape is an error, not a silent
// zero.
func parseTier(tier string) (parsedTier, error) {
	if m := tierCustomRe.FindStringSubmatch(tier); m != nil {
		vcpuN, _ := strconv.ParseFloat(m[2], 64)
		ramMB, _ := strconv.ParseFloat(m[3], 64)
		return parsedTier{VCPU: vcpuN, RAMGB: ramMB / 1024, Edition: editionEnterprise, Family: m[1]}, nil
	}
	if m := tierPerfOptimizedRe.FindStringSubmatch(tier); m != nil {
		vcpuN, _ := strconv.ParseFloat(m[2], 64)
		return parsedTier{VCPU: vcpuN, RAMGB: vcpuN * perfOptimizedRAMPerVCPUGB, Edition: editionEnterprisePlus, Family: m[1]}, nil
	}
	return parsedTier{}, fmt.Errorf("gcp: cost: unrecognized tier shape %q -- expected db-custom-<family>-<vcpu>-<ram_mb> or db-perf-optimized-<family>-<vcpu>", tier)
}

// Calculator computes a monthly on-demand cost estimate for one Cloud SQL
// instance configuration from a pricing.Snapshot's Items. SKUs are matched
// by description keywords (mirroring filterSKUs' own substring-matching
// style), since the Cloud Billing Catalog API has no structured "this SKU is
// the vCPU rate" field. IOPS/throughput overage matching is unverified
// against a live snapshot as of this writing -- run `dbarenactl pricing show
// gcp/cloudsql` and check the matched SKU descriptions before trusting a
// nonzero overage number.
type Calculator struct{}

// Cost implements pricing.Calculator.
func (Calculator) Cost(items []pricing.Item, in pricing.CostInput) (*pricing.CostBreakdown, error) {
	if in.InstanceType == "" {
		return nil, fmt.Errorf("gcp: cost: instance type (tier) is required")
	}
	if in.DiskGB <= 0 {
		return nil, fmt.Errorf("gcp: cost: disk_gb must be positive, got %g", in.DiskGB)
	}
	// DiskType drives storage-SKU disambiguation below: storage descriptions
	// don't repeat the machine family the way vCPU/RAM ones do (e.g.
	// "Enterprise Storage Hyperdisk Balanced Capacity" has no "N4" in it), so
	// it can't be guessed from the tier string -- it must come from the
	// candidate manifest's pricing: disk_type.
	if in.DiskType == "" {
		return nil, fmt.Errorf("gcp: cost: disk_type is required (pricing: disk_type in the candidate manifest, e.g. %q or %q) -- GCP Cloud SQL prices differ by disk type and cannot be guessed from the tier string alone", "HYPERDISK_BALANCED", "PD_SSD")
	}
	if in.DiskType != "HYPERDISK_BALANCED" && in.DiskType != "PD_SSD" {
		return nil, fmt.Errorf("gcp: cost: unsupported disk_type %q -- expected %q or %q", in.DiskType, "HYPERDISK_BALANCED", "PD_SSD")
	}
	pt, err := parseTier(in.InstanceType)
	if err != nil {
		return nil, err
	}

	vcpuItem, err := findOneItem(items, func(it pricing.Item) bool {
		return containsAllFold(it.Description, "vcpu") &&
			descHasEdition(it.Description, pt.Edition) &&
			matchesFamily(it.Description, pt.Family)
	}, fmt.Sprintf("vCPU rate (edition=%s family=%s)", pt.Edition, pt.Family))
	if err != nil {
		return nil, err
	}
	ramItem, err := findOneItem(items, func(it pricing.Item) bool {
		return containsAnyFold(it.Description, "ram", "memory") &&
			descHasEdition(it.Description, pt.Edition) &&
			matchesFamily(it.Description, pt.Family)
	}, fmt.Sprintf("RAM/memory rate (edition=%s family=%s)", pt.Edition, pt.Family))
	if err != nil {
		return nil, err
	}
	storageItem, err := findOneItem(items, func(it pricing.Item) bool {
		return containsAllFold(it.Description, "storage") &&
			!containsAllFold(it.Description, "regional") &&
			!containsAllFold(it.Description, "iops") &&
			!containsAllFold(it.Description, "throughput") &&
			descHasEdition(it.Description, pt.Edition) &&
			matchesDiskTypeKeyword(it.Description, in.DiskType)
	}, fmt.Sprintf("storage rate (edition=%s disk_type=%s)", pt.Edition, in.DiskType))
	if err != nil {
		return nil, err
	}

	baselineIOPS, baselineIOPSSource := defaultBaselineIOPS, fmt.Sprintf("default per %s (Hyperdisk Balanced has no included/free IOPS)", HyperdiskDocsURL)
	if in.DiskBaselineIOPS != nil {
		baselineIOPS, baselineIOPSSource = *in.DiskBaselineIOPS, "pricing.disk_baseline_iops"
	}
	baselineThroughput, baselineThroughputSource := defaultBaselineThroughputMbps, fmt.Sprintf("default per %s (Hyperdisk Balanced has no included/free throughput)", HyperdiskDocsURL)
	if in.DiskBaselineThroughputMbps != nil {
		baselineThroughput, baselineThroughputSource = *in.DiskBaselineThroughputMbps, "pricing.disk_baseline_throughput_mibps"
	}
	iopsOverage := maxFloat(0, in.IOPS-baselineIOPS)
	throughputOverage := maxFloat(0, in.ThroughputMbps-baselineThroughput)

	var iopsUSD, throughputUSD float64
	iopsDetail := fmt.Sprintf("baseline=%g (%s) provisioned=%g overage=%g -- not billed (no matching IOPS SKU needed since overage is 0)", baselineIOPS, baselineIOPSSource, in.IOPS, iopsOverage)
	throughputDetail := fmt.Sprintf("baseline=%g (%s) provisioned=%g overage=%g -- not billed (no matching throughput SKU needed since overage is 0)", baselineThroughput, baselineThroughputSource, in.ThroughputMbps, throughputOverage)
	if iopsOverage > 0 {
		iopsItem, err := findOneItem(items, func(it pricing.Item) bool {
			return containsAllFold(it.Description, "iops") &&
				descHasEdition(it.Description, pt.Edition) &&
				matchesDiskTypeKeyword(it.Description, in.DiskType)
		}, fmt.Sprintf("IOPS overage rate (edition=%s disk_type=%s)", pt.Edition, in.DiskType))
		if err != nil {
			return nil, err
		}
		iopsUSD = iopsItem.PriceUSD * iopsOverage
		iopsDetail = fmt.Sprintf("sku=%s baseline=%g (%s) provisioned=%g overage=%g rate=$%.6f/IOPS-mo = $%.2f",
			iopsItem.SKU, baselineIOPS, baselineIOPSSource, in.IOPS, iopsOverage, iopsItem.PriceUSD, iopsUSD)
	}
	if throughputOverage > 0 {
		throughputItem, err := findOneItem(items, func(it pricing.Item) bool {
			return containsAllFold(it.Description, "throughput") &&
				descHasEdition(it.Description, pt.Edition) &&
				matchesDiskTypeKeyword(it.Description, in.DiskType)
		}, fmt.Sprintf("throughput overage rate (edition=%s disk_type=%s)", pt.Edition, in.DiskType))
		if err != nil {
			return nil, err
		}
		throughputUSD = throughputItem.PriceUSD * throughputOverage
		throughputDetail = fmt.Sprintf("sku=%s baseline=%g MiB/s (%s) provisioned=%g overage=%g rate=$%.6f/MiBps-mo = $%.2f",
			throughputItem.SKU, baselineThroughput, baselineThroughputSource, in.ThroughputMbps, throughputOverage, throughputItem.PriceUSD, throughputUSD)
	}

	var dataCacheUSD float64
	dataCacheDetail := fmt.Sprintf("data_cache_gb=%g -- not billed (no data cache provisioned)", in.DataCacheGB)
	if in.DataCacheGB > 0 {
		dataCacheItem, err := findOneItem(items, func(it pricing.Item) bool {
			if !containsAllFold(it.Description, "data cache") || !descHasEdition(it.Description, pt.Edition) {
				return false
			}
			// Unlike vCPU/RAM, Data Cache SKUs aren't consistently
			// family-qualified: the real catalog's N-series Data Cache row
			// carries no family token at all ("Enterprise Plus Data Cache
			// Storage in Carolina"), while a C4-series decoy explicitly
			// names its family ("Enterprise Plus C4 Data Cache Storage in
			// South Carolina"). Match ours by family when the description
			// does name one, and otherwise accept it as long as it isn't
			// naming a different, known family.
			return matchesFamily(it.Description, pt.Family) || !matchesFamily(it.Description, "C4")
		}, fmt.Sprintf("Data Cache rate (edition=%s family=%s)", pt.Edition, pt.Family))
		if err != nil {
			return nil, err
		}
		dataCacheUSD = dataCacheItem.PriceUSD * in.DataCacheGB
		dataCacheDetail = fmt.Sprintf("sku=%s rate=$%.6f/GiB-mo x %g GB = $%.2f",
			dataCacheItem.SKU, dataCacheItem.PriceUSD, in.DataCacheGB, dataCacheUSD)
	}

	vcpuUSD := vcpuItem.PriceUSD * pt.VCPU * pricing.HoursPerMonth
	ramUSD := ramItem.PriceUSD * pt.RAMGB * pricing.HoursPerMonth
	storageUSD := storageItem.PriceUSD * in.DiskGB

	breakdown := &pricing.CostBreakdown{
		Components: []pricing.CostComponent{
			{
				Name:      "compute_vcpu",
				AmountUSD: vcpuUSD,
				Detail: fmt.Sprintf("sku=%s tier=%s vcpu=%g rate=$%.6f/vcpu-hr x %d hr = $%.2f",
					vcpuItem.SKU, in.InstanceType, pt.VCPU, vcpuItem.PriceUSD, pricing.HoursPerMonth, vcpuUSD),
			},
			{
				Name:      "compute_ram",
				AmountUSD: ramUSD,
				Detail: fmt.Sprintf("sku=%s ram_gb=%g rate=$%.6f/GiB-hr x %d hr = $%.2f",
					ramItem.SKU, pt.RAMGB, ramItem.PriceUSD, pricing.HoursPerMonth, ramUSD),
			},
			{
				Name:      "storage",
				AmountUSD: storageUSD,
				Detail: fmt.Sprintf("sku=%s rate=$%.6f/GiB-mo x %g GB = $%.2f",
					storageItem.SKU, storageItem.PriceUSD, in.DiskGB, storageUSD),
			},
			{Name: "iops_overage", AmountUSD: iopsUSD, Detail: iopsDetail},
			{Name: "throughput_overage", AmountUSD: throughputUSD, Detail: throughputDetail},
			{Name: "data_cache", AmountUSD: dataCacheUSD, Detail: dataCacheDetail},
		},
		TotalUSD: vcpuUSD + ramUSD + storageUSD + iopsUSD + throughputUSD + dataCacheUSD,
	}
	return breakdown, nil
}

// descHasEdition reports whether desc names the given Cloud SQL edition.
// Enterprise Plus descriptions always contain the literal phrase "Enterprise
// Plus"; base Enterprise descriptions contain "Enterprise" but never that
// phrase, so excluding it is what keeps the two editions from overlapping.
func descHasEdition(desc string, ed edition) bool {
	switch ed {
	case editionEnterprisePlus:
		return containsAllFold(desc, "enterprise plus")
	case editionEnterprise:
		return containsAllFold(desc, "enterprise") && !containsAllFold(desc, "enterprise plus")
	default:
		return false
	}
}

// matchesFamily reports whether desc names the given machine family (e.g.
// "N4", "C4", "C4A", "N") as a standalone word. The word boundary is what
// keeps family "N" from matching inside "N4", and family "C4" from matching
// inside "C4A" -- both real, concurrently-priced families in the same
// catalog.
func matchesFamily(desc, family string) bool {
	return regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(family) + `\b`).MatchString(desc)
}

// matchesDiskTypeKeyword reports whether desc is priced for the given disk
// type. Storage descriptions don't repeat the machine family the way
// vCPU/RAM ones do, so disk type -- not family -- is what disambiguates
// storage/IOPS/throughput SKUs across Hyperdisk Balanced, PD-SSD ("Standard
// Storage"), Data Cache, and legacy "Low cost"/Developer-edition storage
// rows that otherwise all contain the word "storage".
func matchesDiskTypeKeyword(desc, diskType string) bool {
	switch diskType {
	case "HYPERDISK_BALANCED":
		return containsAllFold(desc, "hyperdisk") && containsAllFold(desc, "balanced")
	case "PD_SSD":
		return !containsAllFold(desc, "hyperdisk") &&
			!containsAllFold(desc, "data cache") &&
			!containsAllFold(desc, "low cost") &&
			!containsAllFold(desc, "developer")
	default:
		return false
	}
}

func containsAllFold(s string, terms ...string) bool {
	lower := strings.ToLower(s)
	for _, t := range terms {
		if !strings.Contains(lower, strings.ToLower(t)) {
			return false
		}
	}
	return true
}

func containsAnyFold(s string, terms ...string) bool {
	lower := strings.ToLower(s)
	for _, t := range terms {
		if strings.Contains(lower, strings.ToLower(t)) {
			return true
		}
	}
	return false
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
		return pricing.Item{}, fmt.Errorf("gcp: no pricing item found for %s -- run `dbarenactl pricing fetch` again, or check the snapshot with `dbarenactl pricing show gcp`", what)
	case 1:
		return found[0], nil
	default:
		return pricing.Item{}, fmt.Errorf("gcp: %d pricing items matched %s, expected exactly one: %+v", len(found), what, found)
	}
}

package pricing

import "strconv"

// HoursPerMonth is the hours-per-month assumption used to annualize/
// monthly-ize hourly rates across every provider's cost calculation --
// matches the dbarena result schema's own pricing.hours_per_month default.
const HoursPerMonth = 730

// CostInput is the generic, provider-agnostic set of facts needed to price
// one test point's instance configuration. Fields a given provider's
// Calculator doesn't need are simply left zero/empty by the caller.
type CostInput struct {
	// InstanceType identifies the compute SKU to price: AWS's
	// db_instance_type (e.g. "db.t4g.small"), GCP's tier string (e.g.
	// "db-custom-N4-2-4096"), or Supabase's project_size (e.g. "small",
	// which also doubles as its compute SKU).
	InstanceType string
	// DiskType selects among a provider's disk offerings, e.g. "gp3"/"io2".
	// Empty means "use the calculator's own default" (gp3, for AWS/Supabase).
	DiskType       string
	DiskGB         float64
	IOPS           float64
	ThroughputMbps float64
	// DataCacheGB is the provisioned size of a separately-billed data cache
	// (e.g. GCP Cloud SQL Enterprise Plus's Data Cache), when the provider/
	// tier has one. Zero means "no data cache to bill" -- not every provider
	// or tier has this component.
	DataCacheGB float64
	// DiskBaselineIOPS/DiskBaselineThroughputMbps are the free/included disk
	// performance for this specific configuration, when a candidate manifest
	// states one explicitly (the disk_baseline_iops/
	// disk_baseline_throughput_mibps pricing: keys) -- nil means "the
	// calculator should use its own documented default baseline".
	DiskBaselineIOPS           *float64
	DiskBaselineThroughputMbps *float64
}

// CostComponent is one named, itemized part of a CostBreakdown -- exists so
// the pricing audit log can print exactly how a total was assembled without
// every Calculator needing its own logging code.
type CostComponent struct {
	Name      string // e.g. "compute", "storage", "iops_overage", "credit"
	Detail    string // human-readable explanation of the rate/quantity used
	AmountUSD float64
}

// CostBreakdown is the result of a Calculator.Cost call: a total plus every
// component that made it up, in the order they should be presented.
type CostBreakdown struct {
	Components []CostComponent
	TotalUSD   float64
}

// MoneyDecimals is the precision every dollar amount in a CostBreakdown is
// rounded/formatted to -- 3 decimal places (tenths of a cent), matching
// modern cloud-billing sub-cent precision rather than whole cents, which
// would already be lossy for a rate like $0.095/MBps-mo.
const MoneyDecimals = 3

// RoundTo rounds v to decimals decimal places, using the same strconv
// machinery fmt's %.*f verb uses internally -- so RoundTo(v, n) is
// guaranteed to match fmt.Sprintf("%.*f", n, v) exactly, unlike
// math.Round(v*10^n)/10^n, which can round a different direction near
// half-unit boundaries due to its own multiplication error.
func RoundTo(v float64, decimals int) float64 {
	s := strconv.FormatFloat(v, 'f', decimals, 64)
	r, _ := strconv.ParseFloat(s, 64)
	return r
}

// SumComponents totals a breakdown's components' AmountUSD, rounded to
// MoneyDecimals -- keeps a breakdown's TotalUSD exactly equal to the sum of
// what it reports itemized, so the two can never drift or disagree.
func SumComponents(components []CostComponent) float64 {
	var total float64
	for _, c := range components {
		total += c.AmountUSD
	}
	return RoundTo(total, MoneyDecimals)
}

// Calculator computes a monthly on-demand cost estimate for one instance
// configuration from a pricing Snapshot's Items. Implementations must fail
// loudly (return an error) rather than guess when a required SKU/attribute
// is missing from items -- a wrong number here becomes a wrong number in a
// published result.
type Calculator interface {
	Cost(items []Item, input CostInput) (*CostBreakdown, error)
}

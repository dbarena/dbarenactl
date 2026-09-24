package main

// The types below mirror dbarena/dbarena's results/schema/result.schema.json
// exactly (field names, nesting, nullability) -- see that file for the
// authoritative definition. Nullable fields are pointers so an unknown value
// marshals as explicit JSON null rather than being silently omitted or
// defaulted to a zero value that could be mistaken for real data.

type resultDoc struct {
	SchemaVersion   string           `json:"schema_version"`
	Provider        string           `json:"provider"`
	Product         string           `json:"product"`
	Workload        string           `json:"workload"`
	Scenario        string           `json:"scenario"`
	Tier            *string          `json:"tier"`
	BoundType       string           `json:"bound_type"`
	Variant         *string          `json:"variant"`
	Instance        *instanceInfo    `json:"instance,omitempty"`
	MeasuredFrom    string           `json:"measured_from"`
	MeasuredTo      string           `json:"measured_to"`
	Reproducibility reproducibility  `json:"reproducibility"`
	Pricing         *pricingInfo     `json:"pricing"`
	Sweep           []sweepPointJSON `json:"sweep"`
}

type instanceInfo struct {
	InstanceType   *string  `json:"instance_type"`
	VCPU           *float64 `json:"vcpu"`
	RAMGB          *float64 `json:"ram_gb"`
	DiskGB         *float64 `json:"disk_gb"`
	IOPS           *float64 `json:"iops"`
	ThroughputMbps *float64 `json:"throughput_mbps"`
	DiskType       *string  `json:"disk_type"`
	CPUArch        *string  `json:"cpu_arch"`
	EngineVersion  *string  `json:"engine_version"`
	// PgSettings and OrioleDBVersion are omitempty (rather than the
	// explicit-null convention above) because, like sweepPointJSON.Network,
	// they're metadata benchctl only started emitting after older runs
	// already existed -- absent means "not captured", not "unknown".
	PgSettings      *string `json:"pg_settings,omitempty"`
	OrioleDBVersion *string `json:"orioledb_version,omitempty"`
}

type loadGeneratorInfo struct {
	Name    *string `json:"name"`
	Version *string `json:"version"`
}

type reproducibility struct {
	BenchctlVersion   *string           `json:"benchctl_version"`
	DbarenactlVersion *string           `json:"dbarenactl_version"`
	LoadGenerator     loadGeneratorInfo `json:"load_generator"`
	Command           *string           `json:"command"`
}

type pricingInfo struct {
	MonthlyUSD    float64             `json:"monthly_usd"`
	HoursPerMonth float64             `json:"hours_per_month"`
	PricingModel  string              `json:"pricing_model"`
	FetchedAt     *string             `json:"fetched_at"`
	Components    []costComponentJSON `json:"components"`
}

// costComponentJSON mirrors pricing.CostComponent -- one named, itemized
// part of a pricing.monthly_usd total, e.g. "compute", "storage",
// "iops_overage". Discounts/credits are represented the same way, with a
// negative AmountUSD.
type costComponentJSON struct {
	Name      string  `json:"name"`
	AmountUSD float64 `json:"amount_usd"`
	Detail    string  `json:"detail"`
}

type iterationEntry struct {
	Iteration      int     `json:"iteration"`
	StartedAt      string  `json:"started_at"`
	CompletedAt    string  `json:"completed_at"`
	Throughput     float64 `json:"throughput"`
	RawMetricsFile *string `json:"raw_metrics_file"`
	Notes          *string `json:"notes,omitempty"`
}

type throughputInfo struct {
	Metric      string  `json:"metric"`
	Unit        string  `json:"unit"`
	Transaction *string `json:"transaction"`
	Value       float64 `json:"value"`
}

type latencyInfo struct {
	Transaction *string `json:"transaction"`
	P50         float64 `json:"p50"`
	P95         float64 `json:"p95"`
	P99         float64 `json:"p99"`
}

type summaryInfo struct {
	Throughput        throughputInfo `json:"throughput"`
	LatencyMs         latencyInfo    `json:"latency_ms"`
	TpmPerDollarMonth *float64       `json:"tpm_per_dollar_month"`
}

type txnLatencyInfo struct {
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
	P999 float64 `json:"p99_9"`
	Avg  float64 `json:"avg"`
	Max  float64 `json:"max"`
}

type txnMetrics struct {
	Count     float64        `json:"count"`
	Tpm       float64        `json:"tpm"`
	LatencyMs txnLatencyInfo `json:"latency_ms"`
}

type workloadMetrics struct {
	Transactions map[string]txnMetrics `json:"transactions"`
	Errors       map[string]float64    `json:"errors,omitempty"`
}

type sweepPointJSON struct {
	Concurrency        int             `json:"concurrency"`
	WorkloadParameters map[string]any  `json:"workload_parameters,omitempty"`
	Network            *networkInfo    `json:"network,omitempty"`
	LoadDriver         *loadDriverInfo `json:"load_driver,omitempty"`
	// DBSize*/WALSize* are byte counts measured immediately before/after
	// this concurrency level ran. omitempty for the same reason as
	// Network: absent for Results published before this measurement
	// existed.
	DBSizeBefore    *int64           `json:"db_size_before,omitempty"`
	DBSizeAfter     *int64           `json:"db_size_after,omitempty"`
	WALSizeBefore   *int64           `json:"wal_size_before,omitempty"`
	WALSizeAfter    *int64           `json:"wal_size_after,omitempty"`
	Iterations      []iterationEntry `json:"iterations"`
	Summary         summaryInfo      `json:"summary"`
	WorkloadMetrics *workloadMetrics `json:"workload_metrics,omitempty"`
}

// networkInfo is the driver-to-target round trip measured just before this
// concurrency level ran. It describes the path between the load driver and the
// database, not the database hardware, which is why it sits here rather than in
// instance. Omitted for runs made before the probe existed.
type networkInfo struct {
	RTTMinUs    int64 `json:"rtt_min_us"`
	RTTMedianUs int64 `json:"rtt_median_us"`
	RTTP99Us    int64 `json:"rtt_p99_us"`
	RTTMaxUs    int64 `json:"rtt_max_us"`
	Samples     int64 `json:"samples"`
}

// percentileInfo is the p99/p99.9/p99.99 breakdown benchctl's hostmetrics
// package reports for a single driver host metric.
type percentileInfo struct {
	P99   float64 `json:"p99"`
	P999  float64 `json:"p99_9"`
	P9999 float64 `json:"p99_99"`
}

type networkThroughputInfo struct {
	Receive  percentileInfo `json:"receive"`
	Transmit percentileInfo `json:"transmit"`
}

// loadDriverInfo is the load driver's own host metrics (CPU, network),
// captured via Vector while this concurrency level ran. Like networkInfo,
// it's omitted for runs made before this capture existed, or where
// metrics_enabled was false.
type loadDriverInfo struct {
	CPUUtilization               percentileInfo        `json:"cpu_utilization"`
	NetworkThroughputBytesPerSec networkThroughputInfo `json:"network_throughput_bytes_per_sec"`
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

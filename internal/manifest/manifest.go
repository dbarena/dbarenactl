// Package manifest defines dbarenactl's sweep manifest: a declarative,
// human-maintained mapping from a provider's tiers/bound-types/variants to
// concrete benchctl scenario files and --set overrides.
//
// This mapping deliberately lives in data, not Go code: which scenario file
// and which --set values correspond to e.g. "small, io-bound" is exactly the
// kind of benchmark-tuning knowledge that changes as scenarios evolve, and
// baking it into the binary would mean a rebuild every time a scenario
// convention changes.
package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Provider is the cloud/platform a manifest's Product runs on.
const (
	ProviderAWS      = "AWS"
	ProviderGCP      = "GCP"
	ProviderSupabase = "Supabase"
)

// providerInfo holds the static, per-Provider facts dbarenactl needs --
// which pricing fetcher backs it, and whether it has a Plan concept at all.
// One registry instead of several parallel maps keyed by the same Provider
// string, so a new provider's related facts land in one place.
type providerInfo struct {
	// pricingFetcherKey is the id `dbarenactl pricing fetch` looks up in
	// internal/pricing.Registry. Not keyed by (Provider, Product) or Plan:
	// e.g. GCP's Enterprise and Enterprise Plus editions price from the
	// exact same Cloud SQL Billing Catalog SKUs, and Supabase's two
	// products (vanilla, OrioleDB) share the same underlying infra/SKUs --
	// both must resolve to the same Fetcher regardless.
	pricingFetcherKey string
	// hasPlan reports whether this provider has a Plan (edition) concept at
	// all. Plan's specific value is otherwise unconstrained -- see Manifest.
	hasPlan bool
}

var providers = map[string]providerInfo{
	ProviderAWS:      {pricingFetcherKey: "aws/rds", hasPlan: false},
	ProviderGCP:      {pricingFetcherKey: "gcp/cloudsql", hasPlan: true},
	ProviderSupabase: {pricingFetcherKey: "supabase", hasPlan: true},
}

// TestPointDef is one (tier, bound_type, variant) combination to sweep:
// which benchctl scenario to run it against and which --set overrides
// select this specific combination within that scenario.
type TestPointDef struct {
	Tier      string            `yaml:"tier"`
	BoundType string            `yaml:"bound_type"`
	Variant   string            `yaml:"variant,omitempty"`
	Set       map[string]string `yaml:"set"`
	// Pricing holds dbarenactl-internal sizing facts (e.g. disk_type,
	// disk_baseline_iops, disk_baseline_throughput_mibps, data_cache_gb) used by
	// `dbarenactl results`'s cost calculators. Deliberately separate from Set:
	// Set's keys are forwarded verbatim as --set flags to benchctl (see
	// internal/planner), and none of these are inputs any benchctl scenario
	// declares -- passing them there fails with "unknown input". Pricing is
	// never read by the planner/scheduler, only by cmd/dbarenactl/results.go.
	Pricing map[string]string `yaml:"pricing,omitempty"`
}

// Key uniquely identifies a test point within a manifest.
func (d TestPointDef) Key() string {
	if d.Variant == "" {
		return d.Tier + "/" + d.BoundType
	}
	return d.Tier + "/" + d.BoundType + "/" + d.Variant
}

// ParseTestPointRef splits a "tier/bound_type" or "tier/bound_type/variant"
// string -- matching TestPointDef.Key()'s own format exactly -- into its
// parts, e.g. for `dbarenactl run --test-point <ref>`.
func ParseTestPointRef(ref string) (tier, boundType, variant string, err error) {
	parts := strings.Split(ref, "/")
	switch len(parts) {
	case 2:
		return parts[0], parts[1], "", nil
	case 3:
		return parts[0], parts[1], parts[2], nil
	default:
		return "", "", "", fmt.Errorf("invalid test point reference %q -- expected tier/bound_type or tier/bound_type/variant", ref)
	}
}

// FindTestPoint returns the test point matching tier/boundType/variant, if
// any.
func (m *Manifest) FindTestPoint(tier, boundType, variant string) (*TestPointDef, bool) {
	for i := range m.TestPoints {
		d := &m.TestPoints[i]
		if d.Tier == tier && d.BoundType == boundType && d.Variant == variant {
			return d, true
		}
	}
	return nil, false
}

// Manifest describes every test point required for one provider's sweep.
type Manifest struct {
	// Provider is the cloud/platform this manifest covers: one of
	// ProviderAWS, ProviderGCP, ProviderSupabase.
	Provider string `yaml:"provider"`
	// Product is the managed database product under test, e.g. "RDS",
	// "Cloud SQL for Postgres", "Supabase", "OrioleDB" -- free-form and only
	// required to be non-empty, so a new product needs no dbarenactl code
	// change.
	Product string `yaml:"product"`
	// Plan is the product edition/tier, where the product has one, e.g.
	// "Enterprise", "Enterprise Plus", "Pro" -- free-form beyond the
	// structural check of whether this Provider has a Plan concept at all
	// (see providerInfo.hasPlan). Must be empty for a Provider with no plan
	// concept (AWS today).
	Plan string `yaml:"plan,omitempty"`
	// Workload is the benchmark workload name, e.g. "tpcc" -- recorded on
	// every test point and reused when assembling results.
	Workload string `yaml:"workload"`
	// Region is the cloud region this manifest's provider was actually
	// deployed/priced in, e.g. "us-east1". Optional at the manifest level --
	// providers with no per-region pricing (e.g. supabase) never set it --
	// but required by `dbarenactl pricing fetch`/`pricing set` for any
	// provider that does have region-scoped pricing. Deliberately not
	// inferred from a deployment's Terraform default: actual runs can use a
	// different region than a module's `default =` value.
	Region string `yaml:"region,omitempty"`
	// ScenarioPath is the benchctl scenario file, resolved relative to the
	// manifest file's own directory when not absolute.
	ScenarioPath string         `yaml:"scenario_path"`
	TestPoints   []TestPointDef `yaml:"test_points"`

	// resolvedScenarioPath is ScenarioPath after resolving it against the
	// manifest file's directory, always as an absolute path; set by Load.
	// Absolute so that the value can be persisted (e.g. into sweepstate) and
	// later reused correctly regardless of dbarenactl's cwd at that later
	// point.
	resolvedScenarioPath string
}

// ScenarioPath returns the scenario file path resolved against the
// manifest's own directory, always as an absolute path.
func (m *Manifest) ResolvedScenarioPath() string {
	return m.resolvedScenarioPath
}

// PricingFetcherKey returns the provider id `dbarenactl pricing fetch`
// looks up in internal/pricing.Registry -- Provider only, Product and Plan
// deliberately ignored (see providerInfo.pricingFetcherKey's doc comment).
func (m *Manifest) PricingFetcherKey() string {
	return providers[m.Provider].pricingFetcherKey
}

// paramPlaceholderRe matches `{{ params.NAME }}` placeholders in a
// TestPointDef's Set values -- e.g. a required org/account id with no
// sensible default to hardcode in the manifest. Mirrors benchctl's own
// `{{ inputs.NAME }}` scenario templating style (see its
// internal/schema/parse.go's fixtureInputRe) for a familiar syntax, though
// this is a distinct, dbarenactl-side substitution pass: it runs before the
// manifest's `set:` values are ever handed to benchctl.
var paramPlaceholderRe = regexp.MustCompile(`\{\{\s*params\.(\w+)\s*\}\}`)

// ResolveParams substitutes every `{{ params.NAME }}` placeholder across all
// test points' Set values with the corresponding entry in params (populated
// from `dbarenactl run --set NAME=value`), mutating the manifest in place.
// It reports every distinct placeholder left unresolved at once (not just
// the first) with a message telling the caller exactly how to supply it.
func (m *Manifest) ResolveParams(params map[string]string) error {
	missing := map[string]bool{}
	for i := range m.TestPoints {
		for k, v := range m.TestPoints[i].Set {
			m.TestPoints[i].Set[k] = paramPlaceholderRe.ReplaceAllStringFunc(v, func(match string) string {
				name := paramPlaceholderRe.FindStringSubmatch(match)[1]
				if val, ok := params[name]; ok {
					return val
				}
				missing[name] = true
				return match
			})
		}
	}
	if len(missing) == 0 {
		return nil
	}
	names := make([]string, 0, len(missing))
	for name := range missing {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 1 {
		return fmt.Errorf("the parameter %s is required. Set it via --set %s=<value>", names[0], names[0])
	}
	hints := make([]string, len(names))
	for i, name := range names {
		hints[i] = fmt.Sprintf("--set %s=<value>", name)
	}
	return fmt.Errorf("the following parameters are required: %s. Set them via %s",
		strings.Join(names, ", "), strings.Join(hints, " "))
}

// Load reads and validates a manifest file.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("manifest: read %s: %w", path, err)
	}
	m, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("manifest: %s: %w", path, err)
	}
	if filepath.IsAbs(m.ScenarioPath) {
		m.resolvedScenarioPath = m.ScenarioPath
	} else {
		absPath, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("manifest: resolve %s: %w", path, err)
		}
		m.resolvedScenarioPath = filepath.Join(filepath.Dir(absPath), m.ScenarioPath)
	}
	return m, nil
}

// parse decodes and validates manifest content without touching the
// filesystem, so tests can exercise it without writing temp files.
func parse(data []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Manifest) validate() error {
	info, ok := providers[m.Provider]
	if !ok {
		return fmt.Errorf("provider must be one of %s, %s, %s, got %q", ProviderAWS, ProviderGCP, ProviderSupabase, m.Provider)
	}
	if m.Product == "" {
		return fmt.Errorf("product is required")
	}
	if info.hasPlan {
		if m.Plan == "" {
			return fmt.Errorf("provider %q requires a plan", m.Provider)
		}
	} else if m.Plan != "" {
		return fmt.Errorf("provider %q does not use a plan -- remove the plan: field (got %q)", m.Provider, m.Plan)
	}
	if m.Workload == "" {
		return fmt.Errorf("workload is required")
	}
	if m.ScenarioPath == "" {
		return fmt.Errorf("scenario_path is required")
	}
	if len(m.TestPoints) == 0 {
		return fmt.Errorf("test_points must not be empty")
	}
	seen := make(map[string]bool, len(m.TestPoints))
	for i, tp := range m.TestPoints {
		if tp.Tier == "" {
			return fmt.Errorf("test_points[%d]: tier is required", i)
		}
		if tp.BoundType == "" {
			return fmt.Errorf("test_points[%d]: bound_type is required", i)
		}
		key := tp.Key()
		if seen[key] {
			return fmt.Errorf("test_points[%d]: duplicate test point %q", i, key)
		}
		seen[key] = true
	}
	return nil
}

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

// TestPointDef is one (tier, bound_type, variant) combination to sweep:
// which benchctl scenario to run it against and which --set overrides
// select this specific combination within that scenario.
type TestPointDef struct {
	Tier      string            `yaml:"tier"`
	BoundType string            `yaml:"bound_type"`
	Variant   string            `yaml:"variant,omitempty"`
	Set       map[string]string `yaml:"set"`
}

// Key uniquely identifies a test point within a manifest.
func (d TestPointDef) Key() string {
	if d.Variant == "" {
		return d.Tier + "/" + d.BoundType
	}
	return d.Tier + "/" + d.BoundType + "/" + d.Variant
}

// Manifest describes every test point required for one provider's sweep.
type Manifest struct {
	// Provider identifies the provider this manifest covers, e.g. "aws/rds".
	// Matched against dbarenactl run --provider.
	Provider string `yaml:"provider"`
	// Workload is the benchmark workload name, e.g. "tpcc" -- recorded on
	// every test point and reused when assembling results.
	Workload string `yaml:"workload"`
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
	if m.Provider == "" {
		return fmt.Errorf("provider is required")
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

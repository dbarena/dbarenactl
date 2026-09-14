package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// repoRelativePath turns an absolute path into one relative to whichever git
// repository contains it, found generically by walking up from the file
// looking for a `.git` entry -- a file (worktrees like
// benchctl-test-points-no-tiers) or a directory (an ordinary checkout) both
// satisfy this, so it works uniformly for either. Falls back to returning
// absPath unchanged if no `.git` ancestor is found. This is what keeps
// reproducibility fields (scenario_path, the manifest path embedded in
// `command`) portable across machines instead of leaking a local
// /Users/... path -- and automatically follows a scenario/manifest file to
// wherever it's relocated, since it never hardcodes a repo name.
func repoRelativePath(absPath string) string {
	dir := filepath.Dir(absPath)
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			if rel, err := filepath.Rel(dir, absPath); err == nil {
				return filepath.ToSlash(rel)
			}
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return absPath
}

var slugInvalidCharsRe = regexp.MustCompile(`[^a-z0-9]+`)

// slugify mechanically derives a results/index.json-style slug from a
// free-form name, e.g. a candidate's own provider:/product: string --
// lowercased, non-alphanumeric runs collapsed to a single hyphen. Used
// directly (no lookup table) wherever a slug should just track whatever a
// candidate file declares, with no extra registration step per new value:
// "AWS" -> "aws", "GCP" -> "gcp", "Cloud SQL for Postgres" ->
// "cloud-sql-for-postgres".
func slugify(s string) string {
	s = strings.ToLower(s)
	s = slugInvalidCharsRe.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// scenarioSlug folds bound_type + tier [+ variant] into the directory/file
// naming dbarena's results tree uses, e.g. "cache-exceeding-small",
// "cache-exceeding-large-matched-to-rds".
func scenarioSlug(boundType, tier, variant string) string {
	slug := slugify(boundType) + "-" + slugify(tier)
	if variant != "" {
		slug += "-" + slugify(variant)
	}
	return slug
}

// isDbarenaCheckout reports whether dir looks like the root of a dbarena
// checkout: the one thing every checkout is guaranteed to have is its own
// schema file.
func isDbarenaCheckout(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "results", "schema", "result.schema.json"))
	return err == nil
}

// resolveDest implements the --dest auto-detection policy: only two cases
// are checked (cwd itself is a dbarena checkout, or a sibling directory
// literally named "dbarena" is) -- no broader directory walking. Falls back
// to a scratch directory when neither matches and --dest wasn't given.
func resolveDest(explicit, sweepID string) (dir string, isCheckout bool, err error) {
	if explicit != "" {
		return explicit, isDbarenaCheckout(explicit), nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", false, err
	}
	if isDbarenaCheckout(cwd) {
		return cwd, true, nil
	}
	sibling := filepath.Join(cwd, "..", "dbarena")
	if isDbarenaCheckout(sibling) {
		return sibling, true, nil
	}
	return filepath.Join(cwd, "dbarenactl-results-"+sweepID), false, nil
}

// compileResultSchema compiles the target checkout's own
// results/schema/result.schema.json -- never a vendored copy, so validation
// always reflects whatever schema version that checkout actually has.
func compileResultSchema(destRoot string) (*jsonschema.Schema, error) {
	path := filepath.Join(destRoot, "results", "schema", "result.schema.json")
	compiler := jsonschema.NewCompiler()
	return compiler.Compile(path)
}

// validateAgainstSchema checks data (a marshaled result.json) against sch,
// returning a descriptive error naming the scenario on failure.
func validateAgainstSchema(sch *jsonschema.Schema, scenario string, data []byte) error {
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("%s: decode generated result.json for schema validation: %w", scenario, err)
	}
	if err := sch.Validate(v); err != nil {
		return fmt.Errorf("%s: generated result.json failed schema validation: %w", scenario, err)
	}
	return nil
}

// indexEntry is one results/index.json row.
type indexEntry struct {
	Provider string `json:"provider"`
	Product  string `json:"product"`
	Workload string `json:"workload"`
	Scenario string `json:"scenario"`
	// Variant is also folded into Scenario's slug suffix (see scenarioSlug)
	// -- kept here too, as its own property, so callers can filter/group by
	// variant without string-parsing the scenario slug. Omitted when this
	// test point has no variant, matching TestPointDef.Variant's own
	// omitempty convention.
	Variant string `json:"variant,omitempty"`
	Path    string `json:"path"`
}

// updateResultsIndex appends or updates this scenario's entry in
// results/index.json. Entries this call doesn't touch are kept as their
// original raw JSON bytes rather than being decoded and re-marshaled, so an
// unrelated run of `results` never perturbs their key order/formatting --
// only the one entry actually being added or updated changes, keeping any
// resulting diff minimal and reviewable.
func updateResultsIndex(destRoot, provider, product, workload, scenario, variant, relPath string) error {
	indexPath := filepath.Join(destRoot, "results", "index.json")
	var raws []json.RawMessage
	data, err := os.ReadFile(indexPath)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &raws); err != nil {
			return fmt.Errorf("parse %s: %w", indexPath, err)
		}
	case os.IsNotExist(err):
		// No index yet -- start a fresh one.
	default:
		return fmt.Errorf("read %s: %w", indexPath, err)
	}

	updated := indexEntry{Provider: provider, Product: product, Workload: workload, Scenario: scenario, Variant: variant, Path: relPath}
	updatedJSON, err := json.Marshal(updated)
	if err != nil {
		return fmt.Errorf("marshal index entry: %w", err)
	}

	found := false
	for i, raw := range raws {
		var e indexEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			return fmt.Errorf("parse entry %d of %s: %w", i, indexPath, err)
		}
		if e.Provider == provider && e.Product == product && e.Workload == workload && e.Scenario == scenario {
			raws[i] = updatedJSON
			found = true
			break
		}
	}
	if !found {
		raws = append(raws, updatedJSON)
	}

	out, err := json.MarshalIndent(raws, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", indexPath, err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(indexPath, out, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", indexPath, err)
	}
	return nil
}

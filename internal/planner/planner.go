// Package planner turns a loaded sweep manifest into the concrete
// sweepstate rows and benchctl invocations a sweep needs. It intentionally
// contains no per-provider knowledge -- that all lives in the manifest (see
// internal/manifest) -- planner is just the generic glue between "here is a
// declarative list of test points" and "here is what to persist / run".
package planner

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/dbarena/dbarenactl/internal/manifest"
	"github.com/dbarena/dbarenactl/internal/sweepid"
	"github.com/dbarena/dbarenactl/internal/sweepstate"
)

// TestPointID deterministically derives a test point's row id from the
// sweep id and its tier/bound_type/variant. Deterministic (not random) so
// re-running the planner against the same manifest for the same sweep id
// always names the same rows.
func TestPointID(sweepID string, def manifest.TestPointDef) string {
	parts := []string{sweepID, sweepid.Slugify(def.Tier), sweepid.Slugify(def.BoundType)}
	if def.Variant != "" {
		parts = append(parts, sweepid.Slugify(def.Variant))
	}
	return strings.Join(parts, "-")
}

// BuildTestPoints turns every test point in m into a sweepstate.TestPoint
// row for sweepID, applying the same successesNeeded/failureBudget to all of
// them -- the methodology calls for the same number of successful
// iterations across every test point in a sweep.
func BuildTestPoints(sweepID string, m *manifest.Manifest, successesNeeded, failureBudget int) []*sweepstate.TestPoint {
	out := make([]*sweepstate.TestPoint, 0, len(m.TestPoints))
	for _, def := range m.TestPoints {
		out = append(out, &sweepstate.TestPoint{
			ID:              TestPointID(sweepID, def),
			SweepID:         sweepID,
			Tier:            def.Tier,
			Workload:        m.Workload,
			Scenario:        m.ResolvedScenarioPath(),
			BoundType:       def.BoundType,
			Variant:         def.Variant,
			Set:             def.Set,
			SuccessesNeeded: successesNeeded,
			FailureBudget:   failureBudget,
		})
	}
	return out
}

// NewRunID builds a run id for the given test point and 1-based iteration
// attempt. Attempt numbers are not by themselves unique across a test
// point's lifetime -- an orphaned attempt discarded on resume leaves its
// slot's attempt number available for reuse (see the design plan's orphan
// handling) -- so a short random suffix guarantees no collision with a
// run id benchctl already has a record for, matching the uniqueness benchctl
// itself now enforces on caller-supplied --run-id values.
func NewRunID(testPointID string, attempt int) string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return testPointID + "-" + strconv.Itoa(attempt) + "-" + hex.EncodeToString(b[:])
}

// LaunchCommand describes one `benchctl run --async` invocation a dry run
// would print, or a real run would execute.
type LaunchCommand struct {
	TestPointKey string
	RunID        string
	ScenarioPath string
	Set          map[string]string
}

// String renders the command the way a user would type it, for --dry-run
// output. Flags are sorted for stable, diffable output.
func (c LaunchCommand) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "benchctl run --async --run-id %s %s", c.RunID, c.ScenarioPath)
	keys := make([]string, 0, len(c.Set))
	for k := range c.Set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, " --set %s=%s", k, c.Set[k])
	}
	return b.String()
}

// PreviewFirstAttempts returns one LaunchCommand per test point in m,
// representing the first iteration's invocation -- used by `dbarenactl run
// --dry-run`. Later iterations follow the same shape under a new run id;
// how many actually happen depends on runtime success/failure and isn't
// enumerable ahead of time.
func PreviewFirstAttempts(sweepID string, m *manifest.Manifest) []LaunchCommand {
	out := make([]LaunchCommand, 0, len(m.TestPoints))
	for _, def := range m.TestPoints {
		tpID := TestPointID(sweepID, def)
		out = append(out, LaunchCommand{
			TestPointKey: def.Key(),
			RunID:        NewRunID(tpID, 1),
			ScenarioPath: m.ResolvedScenarioPath(),
			Set:          def.Set,
		})
	}
	return out
}

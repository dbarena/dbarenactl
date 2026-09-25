package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dbarena/dbarenactl/internal/bench"
	"github.com/dbarena/dbarenactl/internal/sweepstate"
)

var statusAll bool

var statusCmd = &cobra.Command{
	Use:   "status [sweep-id]",
	Short: "Show local sweep progress, merged with live benchctl status for active runs",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runStatusCmd,
}

func init() {
	statusCmd.Flags().BoolVar(&statusAll, "all", false, "Include completed sweeps in the list (only applies with no sweep-id given)")
}

func runStatusCmd(cmd *cobra.Command, args []string) error {
	dbFile, err := dbPath()
	if err != nil {
		return err
	}
	store, err := sweepstate.Open(dbFile)
	if err != nil {
		return err
	}
	defer store.Close() //nolint:errcheck

	if len(args) == 0 {
		if statusAll {
			sweeps, err := store.ListAllSweeps()
			if err != nil {
				return err
			}
			if len(sweeps) == 0 {
				fmt.Println("no sweeps")
				return nil
			}
			return printSweepList(store, sweeps)
		}
		return listIncompleteSweeps(store)
	}

	sweepID := args[0]
	sweep, err := store.GetSweep(sweepID)
	if err != nil {
		return fmt.Errorf("sweep %s: %w", sweepID, err)
	}
	testPoints, err := store.ListTestPoints(sweepID)
	if err != nil {
		return err
	}

	fmt.Printf("Sweep:    %s\n", sweep.ID)
	fmt.Printf("Provider: %s\n", sweep.Provider)
	fmt.Printf("Product:  %s\n", sweep.Product)
	fmt.Printf("Plan:     %s\n", dashIfEmpty(sweep.Plan))
	fmt.Printf("Workload: %s\n", sweep.Workload)
	fmt.Printf("Status:   %s\n", sweep.Status)
	if sweep.HasError() {
		fmt.Printf("Stopped:  %s failed on %s\n          %s\n", sweep.ErrorAction, sweep.ErrorTarget, sweep.ErrorDetail)
	}

	p, err := computeProgress(store, sweep)
	if err != nil {
		return err
	}
	fmt.Printf("Progress: [%d/%d] runs done\n", p.Done, p.Total)
	if p.HasETA {
		finish := time.Now().Add(p.ETA).Local().Format("2006-01-02 15:04")
		fmt.Printf("ETA:      %s remaining (around %s)\n", formatDuration(p.ETA), finish)
	}

	fmt.Println("\nTest points:")
	if err := printTestPointTable(os.Stdout, testPoints); err != nil {
		return err
	}

	runs, err := store.ListNonTerminalRuns(sweepID)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		return nil
	}
	avgDurations, err := successAvgDurationsByTestPoint(store, sweepID)
	if err != nil {
		return err
	}
	fmt.Println("\nActive runs:")
	rw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(rw, "  RUN ID\tSTART\tDBARENA STATUS\tETA")
	for _, r := range runs {
		start := r.CreatedAt.Local().Format("2006-01-02 15:04")
		eta := "-"
		if avg, ok := avgDurations[r.TestPointID]; ok {
			remaining := avg - time.Since(r.CreatedAt)
			if remaining < 0 {
				remaining = 0
			}
			eta = formatDuration(remaining)
		}
		fmt.Fprintf(rw, "  %s\t%s\t%s\t%s\n", r.RunID, start, r.Status, eta)
	}
	return rw.Flush()
}

// printTestPointTable renders one row per test point (label, successes,
// failures against their targets) to w, indented to match the rest of
// `status`'s output. Shared with `resume`'s budget-exhausted recovery
// prompt, which needs the same "what has results, what's missing" view.
func printTestPointTable(w io.Writer, testPoints []*sweepstate.TestPoint) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  TEST POINT\tSUCCESSES\tFAILURES")
	for _, tp := range testPoints {
		failures := fmt.Sprintf("%d/%d", tp.FailuresCount, tp.FailureBudget)
		if tp.Skipped {
			failures += " (skipped)"
		}
		fmt.Fprintf(tw, "  %s\t%d/%d\t%s\n", testPointLabel(tp), tp.SuccessesCount, tp.SuccessesNeeded, failures)
	}
	return tw.Flush()
}

func testPointLabel(tp *sweepstate.TestPoint) string {
	return tp.Label()
}

// progress tracks a sweep's completed-vs-total successful run count (e.g. 10
// test points needing 3 successful iterations each is 30 runs total) and,
// once there's enough data, an estimate of how much longer it will take.
type progress struct {
	Done, Total int
	ETA         time.Duration
	HasETA      bool
}

// computeProgress counts a sweep's successful runs against the total
// requires runs. If the sweep is still running we derive a best effort
// ETA if each remaining test point has at least one successful run to
// estimate a duration from, an ETA for the rest of the sweep.
func computeProgress(store *sweepstate.Store, sweep *sweepstate.Sweep) (progress, error) {
	testPoints, err := store.ListTestPoints(sweep.ID)
	if err != nil {
		return progress{}, err
	}

	var p progress
	for _, tp := range testPoints {
		needed := tp.SuccessesNeeded
		if tp.Skipped {
			needed = tp.SuccessesCount
		}
		p.Total += needed
		p.Done += tp.SuccessesCount
	}

	if sweep.Status != sweepstate.SweepRunning || p.Done == p.Total {
		return p, nil
	}

	runs, err := store.ListRunsForSweep(sweep.ID)
	if err != nil {
		return progress{}, err
	}
	avgDurations := avgDurationsFromRuns(runs)
	now := time.Now()
	inFlightElapsed := map[string][]time.Duration{}
	for _, r := range runs {
		if !r.Status.Terminal() {
			inFlightElapsed[r.TestPointID] = append(inFlightElapsed[r.TestPointID], now.Sub(r.CreatedAt))
		}
	}

	var remaining time.Duration
	for _, tp := range testPoints {
		if tp.Skipped || tp.Satisfied() {
			continue
		}
		avg, ok := avgDurations[tp.ID]
		if !ok {
			// Not enough data yet to estimate this test point's duration, so
			// not enough to estimate the sweep's remaining time either.
			return p, nil
		}
		attemptsLeft := tp.SuccessesNeeded - tp.SuccessesCount
		grossWork := avg * time.Duration(attemptsLeft)

		// Credit runs already in flight for this test point with the time
		// they've already spent, capped at avg per run so an overrunning run
		// can't push this test point's contribution negative.
		var credit time.Duration
		for _, elapsed := range inFlightElapsed[tp.ID] {
			if elapsed > avg {
				elapsed = avg
			}
			credit += elapsed
		}

		tpRemaining := grossWork - credit
		if tpRemaining < 0 {
			tpRemaining = 0
		}
		remaining += tpRemaining
	}

	concurrency := 1
	var params sweepParams
	if err := json.Unmarshal([]byte(sweep.ParamsJSON), &params); err == nil && params.MaxConcurrency > 0 {
		concurrency = params.MaxConcurrency
	}

	p.ETA = remaining / time.Duration(concurrency)
	p.HasETA = true
	return p, nil
}

// avgDurationsFromRuns returns each test point's average duration across its
// successful terminal runs, keyed by test point id, for test points with at
// least one such run.
func avgDurationsFromRuns(runs []*sweepstate.Run) map[string]time.Duration {
	sums := map[string]time.Duration{}
	counts := map[string]int{}
	for _, r := range runs {
		if r.Outcome == "success" {
			sums[r.TestPointID] += r.UpdatedAt.Sub(r.CreatedAt)
			counts[r.TestPointID]++
		}
	}
	avgs := make(map[string]time.Duration, len(sums))
	for id, sum := range sums {
		avgs[id] = sum / time.Duration(counts[id])
	}
	return avgs
}

// successAvgDurationsByTestPoint is avgDurationsFromRuns for every run in
// sweepID, for callers (e.g. the active-runs table) that haven't already
// loaded the sweep's runs themselves.
func successAvgDurationsByTestPoint(store *sweepstate.Store, sweepID string) (map[string]time.Duration, error) {
	runs, err := store.ListRunsForSweep(sweepID)
	if err != nil {
		return nil, err
	}
	return avgDurationsFromRuns(runs), nil
}

// formatDuration renders d as e.g. "2h15m" or "45m" ("<1m" for anything
// under a minute), or, once d reaches a full day, in days and hours instead
// (e.g. "2d5h").
func formatDuration(d time.Duration) string {
	if d >= 24*time.Hour {
		d = d.Round(time.Hour)
		days := d / (24 * time.Hour)
		hours := (d % (24 * time.Hour)) / time.Hour
		if hours == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd%dh", days, hours)
	}
	d = d.Round(time.Minute)
	if d < time.Minute {
		return "<1m"
	}
	h := d / time.Hour
	m := (d % time.Hour) / time.Minute
	if h == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

// currentPhase returns the last phase with any recorded status, for a
// compact one-line summary.
func currentPhase(rs *bench.RunState) string {
	order := []string{
		bench.PhaseTeardown, bench.PhaseDriverCollect, bench.PhaseWorkloadExecute,
		bench.PhaseWorkloadPrepare, bench.PhaseDriverSetup, bench.PhaseProvision,
	}
	for _, p := range order {
		if st := rs.Phases[p]; st != "" && st != "pending" {
			return p + ": " + st
		}
	}
	return "pending"
}

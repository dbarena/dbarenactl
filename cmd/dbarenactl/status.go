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
	"github.com/dbarena/dbarenactl/internal/scheduler"
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
		fmt.Printf("Stopped:  %s failed on %s\n          %s\n",
			sweep.ErrorAction, errorTargetLabel(store, sweep.ErrorTarget), sweep.ErrorDetail)
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
	allRuns, err := store.ListRunsForSweep(sweepID)
	if err != nil {
		return err
	}
	avgDurations := scheduler.AvgSuccessDurations(allRuns)
	// TEST POINT first, and paired with RUN ID: `run`'s progress output
	// names runs by their short label only, so this table is where a label
	// seen there is mapped back to the run id `benchctl` commands take.
	fmt.Println("\nActive runs:")
	rw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(rw, "  TEST POINT\tRUN ID\tSTART\tDBARENA STATUS\tETA")
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
		label := "-"
		if tp, err := store.GetTestPoint(r.TestPointID); err == nil {
			label = tp.RunLabel(r.IterationAttempt)
		}
		fmt.Fprintf(rw, "  %s\t%s\t%s\t%s\t%s\n", label, r.RunID, start, r.Status, eta)
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
// required runs. While the sweep is running, it also estimates the remaining
// time by simulating the scheduler (see scheduler.EstimateRemaining), once
// every remaining test point has a successful run to estimate from.
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
	concurrency := 1
	var params sweepParams
	if err := json.Unmarshal([]byte(sweep.ParamsJSON), &params); err == nil && params.MaxConcurrency > 0 {
		concurrency = params.MaxConcurrency
	}
	p.ETA, p.HasETA = scheduler.EstimateRemaining(testPoints, runs, time.Now(), concurrency)
	return p, nil
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

package main

import (
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dbarena/dbarenactl/internal/bench"
	"github.com/dbarena/dbarenactl/internal/sweepstate"
)

var statusBenchctlBin string

var statusCmd = &cobra.Command{
	Use:   "status [sweep-id]",
	Short: "Show local sweep progress, merged with live benchctl status for active runs",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runStatusCmd,
}

func init() {
	statusCmd.Flags().StringVar(&statusBenchctlBin, "benchctl-bin", "benchctl", "Path to the benchctl binary")
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
	fmt.Println("\nActive runs:")
	c := bench.New(statusBenchctlBin)
	rw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(rw, "  RUN ID\tDBARENA STATUS\tBENCHCTL PHASE")
	for _, r := range runs {
		phase := "-"
		if rs, err := c.Status(cmd.Context(), r.RunID); err == nil {
			phase = currentPhase(rs)
		}
		fmt.Fprintf(rw, "  %s\t%s\t%s\n", r.RunID, r.Status, phase)
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
		fmt.Fprintf(tw, "  %s\t%d/%d\t%d/%d\n", testPointLabel(tp), tp.SuccessesCount, tp.SuccessesNeeded, tp.FailuresCount, tp.FailureBudget)
	}
	return tw.Flush()
}

func testPointLabel(tp *sweepstate.TestPoint) string {
	if tp.Variant == "" {
		return tp.Tier + "/" + tp.BoundType
	}
	return tp.Tier + "/" + tp.BoundType + "/" + tp.Variant
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

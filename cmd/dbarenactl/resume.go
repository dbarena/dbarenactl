package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dbarena/dbarenactl/internal/bench"
	"github.com/dbarena/dbarenactl/internal/lock"
	"github.com/dbarena/dbarenactl/internal/scheduler"
	"github.com/dbarena/dbarenactl/internal/sweepstate"
)

var resumeBenchctlBin string

var resumeCmd = &cobra.Command{
	Use:   "resume [sweep-id]",
	Short: "Continue an incomplete sweep, or list incomplete sweeps if no id is given",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runResume,
}

func init() {
	resumeCmd.Flags().StringVar(&resumeBenchctlBin, "benchctl-bin", "benchctl", "Path to the benchctl binary")
}

func runResume(cmd *cobra.Command, args []string) error {
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
	if sweep.Status == sweepstate.SweepCompleted {
		fmt.Printf("sweep %s is already complete\n", sweepID)
		return nil
	}
	if sweep.ErrorAction == scheduler.ActionBudgetExhausted {
		if err := recoverBudgetExhausted(cmd, store, sweep, resumeBenchctlBin); err != nil {
			return err
		}
	} else if sweep.HasError() {
		printResumeNotice(sweep)
	}

	return executeSweep(cmd.Context(), store, sweepID, resumeBenchctlBin)
}

// recoverBudgetExhausted presents the sweep's exact per-test-point state
// (which have results, which are missing/exhausted) and asks the user,
// interactively, whether to continue -- keeping every existing result and
// giving the exhausted test point(s) a fresh failure allowance for
// whatever successes they still need -- or to discard the whole sweep's
// progress and start over. Either way, `run`/`executeSweep`'s own
// reconciliation loop picks up normally once this returns; this only
// un-stops the sweep and adjusts its recorded state.
func recoverBudgetExhausted(cmd *cobra.Command, store *sweepstate.Store, sweep *sweepstate.Sweep, benchctlBin string) error {
	testPoints, err := store.ListTestPoints(sweep.ID)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Sweep %s stopped: test point %s exhausted its failure budget:\n  %s\n\n", sweep.ID, sweep.ErrorTarget, sweep.ErrorDetail)
	fmt.Fprintln(os.Stderr, "Current state:")
	if err := printTestPointTable(os.Stderr, testPoints); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr)

	choice, err := promptContinueOrRestart(cmd.InOrStdin(), os.Stderr, sweep.ID)
	if err != nil {
		return err
	}

	lockFilePath, err := lockPath(sweep.ID)
	if err != nil {
		return err
	}
	l, err := lock.Acquire(lockFilePath)
	if err != nil {
		if errors.Is(err, lock.ErrHeld) {
			return fmt.Errorf("sweep %s is already being managed by another dbarenactl process: %w", sweep.ID, err)
		}
		return err
	}
	defer l.Release() //nolint:errcheck

	switch choice {
	case recoveryContinue:
		if err := store.ExtendExhaustedTestPoints(sweep.ID); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Continuing -- existing results kept; the exhausted test point(s) get a fresh failure allowance for whatever successes they still need.")
	case recoveryRestart:
		logDir, err := logsBaseDir(sweep.ID)
		if err != nil {
			return err
		}
		benchClient := bench.New(benchctlBin)
		benchClient.LogDir = logDir
		sched := &scheduler.Scheduler{Store: store, Bench: benchClient, Out: os.Stderr}
		if err := sched.RestartExhaustedSweep(cmd.Context(), sweep.ID); err != nil {
			return err
		}
		msg := "Starting fresh -- every test point's progress discarded, redoing the whole sweep"
		if dir, err := artifactBaseDir(sweep.ID); err == nil {
			msg += fmt.Sprintf(" (prior result files untouched on disk under %s)", dir)
		}
		fmt.Fprintln(os.Stderr, msg+".")
	}
	return nil
}

type recoveryChoice int

const (
	recoveryContinue recoveryChoice = iota
	recoveryRestart
)

// promptContinueOrRestart asks the user, over in/out, to decide how to
// recover a budget-exhausted sweep. Reads from in via a line scanner so
// tests can supply a plain strings.Reader instead of a real terminal.
func promptContinueOrRestart(in io.Reader, out io.Writer, sweepID string) (recoveryChoice, error) {
	scanner := bufio.NewScanner(in)
	for attempt := 0; attempt < 3; attempt++ {
		fmt.Fprint(out, "Continue with existing results and extend the budget [c], or discard all progress and start fresh [f]? ")
		if !scanner.Scan() {
			return 0, fmt.Errorf("no input received -- re-run `dbarenactl resume %s` from an interactive terminal to decide", sweepID)
		}
		switch strings.ToLower(strings.TrimSpace(scanner.Text())) {
		case "c", "continue":
			return recoveryContinue, nil
		case "f", "fresh", "restart":
			return recoveryRestart, nil
		}
		fmt.Fprintln(out, "please answer 'c' or 'f'")
	}
	return 0, fmt.Errorf("no valid answer after 3 attempts")
}

func listIncompleteSweeps(store *sweepstate.Store) error {
	sweeps, err := store.ListIncompleteSweeps()
	if err != nil {
		return err
	}
	if len(sweeps) == 0 {
		fmt.Println("no incomplete sweeps")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SWEEP\tPROVIDER\tPRODUCT\tPLAN\tWORKLOAD\tSTATUS")
	for _, sw := range sweeps {
		status := string(sw.Status)
		if sw.HasError() {
			status = fmt.Sprintf("stopped (%s: %s)", sw.ErrorAction, sw.ErrorDetail)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", sw.ID, sw.Provider, sw.Product, dashIfEmpty(sw.Plan), sw.Workload, status)
	}
	return w.Flush()
}

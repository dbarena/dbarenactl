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

var (
	resumeBenchctlBin    string
	resumeMaxConcurrency int
)

var resumeCmd = &cobra.Command{
	Use:   "resume [sweep-id]",
	Short: "Continue an incomplete sweep, or list incomplete sweeps if no id is given",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runResume,
}

func init() {
	resumeCmd.Flags().StringVar(&resumeBenchctlBin, "benchctl-bin", "benchctl", "Path to the benchctl binary")
	resumeCmd.Flags().IntVar(&resumeMaxConcurrency, "max-concurrency", 0,
		"Override this sweep's concurrency, persisted for future resumes too (0 = keep the current value)")
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

	return executeSweep(cmd.Context(), store, sweepID, resumeBenchctlBin, resumeMaxConcurrency)
}

// recoverBudgetExhausted presents the sweep's exact per-test-point state
// (which have results, which are missing/exhausted) and asks the user,
// interactively, in two stages: first whether to continue (keeping every
// existing result) or discard the whole sweep's progress and start over;
// then, only if continuing, whether to give the specific exhausted test
// point a fresh failure allowance and keep retrying it, or skip it
// permanently and let the rest of the sweep finish without it. Either way,
// `run`/`executeSweep`'s own reconciliation loop picks up normally once this
// returns; this only un-stops the sweep and adjusts its recorded state.
func recoverBudgetExhausted(cmd *cobra.Command, store *sweepstate.Store, sweep *sweepstate.Sweep, benchctlBin string) error {
	testPoints, err := store.ListTestPoints(sweep.ID)
	if err != nil {
		return err
	}
	exhaustedTP, err := store.GetTestPoint(sweep.ErrorTarget)
	if err != nil {
		return err
	}

	if err := printBudgetExhaustedDiagnostic(os.Stderr, store, sweep.ID, exhaustedTP); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Current state:")
	if err := printTestPointTable(os.Stderr, testPoints); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr)

	choice, err := promptContinueOrRestart(cmd.InOrStdin(), os.Stderr, sweep.ID)
	if err != nil {
		return err
	}

	var retryOrSkip retryOrSkipChoice
	if choice == recoveryContinue {
		retryOrSkip, err = promptRetryOrSkip(cmd.InOrStdin(), os.Stderr, exhaustedTP)
		if err != nil {
			return err
		}
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
		switch retryOrSkip {
		case choiceRetry:
			if err := store.ExtendExhaustedTestPoints(sweep.ID); err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "Continuing.")
		case choiceSkip:
			if err := store.SkipTestPoint(sweep.ID, exhaustedTP.ID); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Skipped test point %s. To run a new sweep for just this test point use `--test-point %s`.\n",
				exhaustedTP.ID, testPointLabel(exhaustedTP))
		}
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
		msg := "Discarding every test point's progress and starting fresh"
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
		fmt.Fprint(out, "Continue with existing results [c], or discard all progress and start fresh [f]? ")
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

type retryOrSkipChoice int

const (
	choiceRetry retryOrSkipChoice = iota
	choiceSkip
)

// promptRetryOrSkip asks the user, only after they've chosen to continue a
// budget-exhausted sweep, what to do about the one test point that actually
// exhausted its budget: keep retrying it with a fresh failure allowance, or
// give up on it permanently and let the rest of the sweep finish without it.
// The prompt itself names the exact `--test-point` value to reuse later, so
// skipping is visibly not a dead end.
//
// Unlike promptContinueOrRestart, no input at all (as opposed to a typed but
// invalid answer) doesn't error: it defaults to retrying, the conservative,
// non-destructive choice, and the exact behavior a script that only ever
// answered the old single continue/fresh prompt already relied on -- it
// shouldn't start failing on a question it doesn't know exists.
func promptRetryOrSkip(in io.Reader, out io.Writer, tp *sweepstate.TestPoint) (retryOrSkipChoice, error) {
	scanner := bufio.NewScanner(in)
	label := testPointLabel(tp)
	for attempt := 0; attempt < 3; attempt++ {
		fmt.Fprintf(out, "Keep retrying from %s [r], or skip it [s]? You can start a new sweep later for just this test point with `--test-point %s`. ",
			tp.ID, label)
		if !scanner.Scan() {
			fmt.Fprintf(out, "\nNo further input -- keeping %s and extending its failure budget. Run `dbarenactl resume %s` again from an interactive terminal to skip it instead.\n", tp.ID, tp.SweepID)
			return choiceRetry, nil
		}
		switch strings.ToLower(strings.TrimSpace(scanner.Text())) {
		case "r", "retry":
			return choiceRetry, nil
		case "s", "skip":
			return choiceSkip, nil
		}
		fmt.Fprintln(out, "please answer 'r' or 's'")
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
	return printSweepList(sweeps)
}

// printSweepList renders one row per sweep (id, provider, product, plan,
// workload, status, and when it was last started -- initial run or most
// recent resume, whichever is later), most recently started first (see
// Store.ListIncompleteSweeps/ListAllSweeps). Shared by `resume`'s
// incomplete-only list and `status --all`'s full list.
func printSweepList(sweeps []*sweepstate.Sweep) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "STARTED\tSWEEP\tPROVIDER\tPRODUCT\tPLAN\tWORKLOAD\tSTATUS")
	for _, sw := range sweeps {
		status := string(sw.Status)
		if sw.HasError() {
			status = fmt.Sprintf("stopped (%s: %s)", sw.ErrorAction, sw.ErrorDetail)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", sw.LastStartedAt.Local().Format("2006-01-02 15:04"),
			sw.ID, sw.Provider, sw.Product, dashIfEmpty(sw.Plan), sw.Workload, status)
	}
	return w.Flush()
}

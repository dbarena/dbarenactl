package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dbarena/dbarenactl/internal/bench"
	"github.com/dbarena/dbarenactl/internal/lock"
	"github.com/dbarena/dbarenactl/internal/sweepstate"
	"github.com/dbarena/dbarenactl/internal/ui"
)

var (
	deleteBenchctlBin string
	deleteYes         bool
)

var deleteCmd = &cobra.Command{
	Use:   "delete <sweep-id>",
	Short: "Tear down any live environments and permanently delete a sweep's state",
	Args:  cobra.ExactArgs(1),
	RunE:  runDelete,
}

func init() {
	deleteCmd.Flags().StringVar(&deleteBenchctlBin, "benchctl-bin", "benchctl", "Path to the benchctl binary")
	deleteCmd.Flags().BoolVarP(&deleteYes, "yes", "y", false, "Skip the confirmation prompt (for scripting/CI)")
}

func runDelete(cmd *cobra.Command, args []string) error {
	sweepID := args[0]

	dbFile, err := dbPath()
	if err != nil {
		return err
	}
	store, err := sweepstate.Open(dbFile)
	if err != nil {
		return err
	}
	defer store.Close() //nolint:errcheck

	if _, err := store.GetSweep(sweepID); err != nil {
		return fmt.Errorf("sweep %s: %w", sweepID, err)
	}

	// Taken before the prompt so a sweep another process is currently
	// managing fails fast, rather than after the user has answered.
	lockFilePath, err := lockPath(sweepID)
	if err != nil {
		return err
	}
	l, err := lock.Acquire(lockFilePath)
	if err != nil {
		if errors.Is(err, lock.ErrHeld) {
			return fmt.Errorf("sweep %s is already being managed by another dbarenactl process: %w", sweepID, err)
		}
		return err
	}
	defer l.Release() //nolint:errcheck

	runs, err := store.ListNonTerminalRuns(sweepID)
	if err != nil {
		return err
	}

	logDir, err := logsBaseDir(sweepID)
	if err != nil {
		return err
	}
	benchClient := bench.New(deleteBenchctlBin)
	benchClient.LogDir = logDir

	// dbarenactl's own bookkeeping only says a run hasn't been fetched/torn
	// down *by dbarenactl*; benchctl's own state is the source of truth for
	// whether an environment is actually still up (e.g. someone may have run
	// `benchctl teardown --purge` on it by hand). Ask benchctl about each
	// candidate before deciding what to warn about or tear down. That is one
	// subprocess per run, so a sweep with several live environments sits here
	// for a while before there is anything to print -- hence the spinner.
	var (
		live        []liveRun
		alreadyGone int
	)
	if len(runs) > 0 {
		sp := ui.New(fmt.Sprintf("Checking %d environment(s)", len(runs)))
		sp.Start()
		live, alreadyGone = classifyRuns(cmd.Context(), benchClient, runs)
		sp.Succeed(fmt.Sprintf("Checked %d environment(s): %d still running", len(runs), len(live)))
	}

	if len(live) > 0 && !deleteYes {
		fmt.Fprintf(os.Stderr, "warning: sweep %s still has %d running environment(s):\n", sweepID, len(live))
		printLiveRunTable(os.Stderr, live)
		if alreadyGone > 0 {
			fmt.Fprintf(os.Stderr, "(%d other locally-tracked run(s) are no longer known to benchctl and will just be cleaned up)\n", alreadyGone)
		}
		confirmed, err := promptYesNo(cmd.InOrStdin(), os.Stderr,
			fmt.Sprintf("Tear down these environments and delete sweep %s? [y/N] ", sweepID))
		if err != nil {
			return fmt.Errorf("sweep %s: %w", sweepID, err)
		}
		if !confirmed {
			return fmt.Errorf("aborted: sweep %s not deleted", sweepID)
		}
	}

	for _, lr := range live {
		// Tearing down one environment takes minutes, so it gets the same
		// spinner treatment the scheduler gives its own teardowns.
		sp := ui.New(fmt.Sprintf("%s: tearing down", lr.run.RunID))
		sp.Start()
		start := time.Now()
		// A teardown failure is reported but never blocks the deletion: the
		// point of `delete` is to get rid of the sweep, and an environment
		// that can't be torn down here needs manual attention anyway (see
		// docs/troubleshooting.md).
		if err := benchClient.Teardown(cmd.Context(), lr.run.RunID); err != nil {
			sp.Fail(fmt.Sprintf("%s: tear down failed: %v", lr.run.RunID, err))
			continue
		}
		sp.Succeed(fmt.Sprintf("%s: torn down in %s", lr.run.RunID, time.Since(start).Round(time.Second)))
	}

	if err := store.DeleteSweep(sweepID); err != nil {
		return err
	}
	fmt.Printf("sweep %s deleted\n", sweepID)
	return nil
}

// liveRun pairs a non-terminal run with the phase benchctl last reported for
// it, cached from the single Status call classifyRuns already made -- so
// displaying it doesn't need a second round-trip.
type liveRun struct {
	run   *sweepstate.Run
	phase string
}

// classifyRuns asks benchctl about each of a sweep's locally non-terminal
// runs and splits them into runs that genuinely still need tearing down
// ("live") and ones benchctl no longer has any record of at all -- e.g. torn
// down by hand with `benchctl teardown --purge`, or otherwise already gone
// -- which need no teardown call and shouldn't be reported as still running.
// A run whose status can't be determined (a transient error, not
// ErrRunNotFound) is conservatively treated as live: better to attempt a
// teardown that turns out to be a no-op than to silently skip one that
// wasn't.
func classifyRuns(ctx context.Context, c *bench.Client, runs []*sweepstate.Run) (live []liveRun, alreadyGone int) {
	for _, r := range runs {
		rs, err := c.Status(ctx, r.RunID)
		switch {
		case errors.Is(err, bench.ErrRunNotFound):
			alreadyGone++
		case err != nil:
			live = append(live, liveRun{run: r, phase: fmt.Sprintf("? (status check failed: %v)", err)})
		case rs.TerminatedAt != nil:
			alreadyGone++
		default:
			live = append(live, liveRun{run: r, phase: currentPhase(rs)})
		}
	}
	return live, alreadyGone
}

// printLiveRunTable renders the same live view of a sweep's still-running
// runs that `status` shows under "Active runs", so the user sees exactly
// what is about to be torn down before confirming.
func printLiveRunTable(w io.Writer, live []liveRun) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  RUN ID\tDBARENA STATUS\tBENCHCTL PHASE")
	for _, lr := range live {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", lr.run.RunID, lr.run.Status, lr.phase)
	}
	tw.Flush() //nolint:errcheck
}

// promptYesNo asks the user, over in/out, to confirm an irreversible
// action. Reads from in via a line scanner so tests can supply a plain
// strings.Reader instead of a real terminal. Anything but an explicit yes
// -- including an empty answer -- means no.
func promptYesNo(in io.Reader, out io.Writer, prompt string) (bool, error) {
	scanner := bufio.NewScanner(in)
	for attempt := 0; attempt < 3; attempt++ {
		fmt.Fprint(out, prompt)
		if !scanner.Scan() {
			return false, fmt.Errorf("no input received -- re-run from an interactive terminal to confirm, or pass --yes")
		}
		switch strings.ToLower(strings.TrimSpace(scanner.Text())) {
		case "y", "yes":
			return true, nil
		case "n", "no", "":
			return false, nil
		}
		fmt.Fprintln(out, "please answer 'y' or 'n'")
	}
	return false, fmt.Errorf("no valid answer after 3 attempts")
}

package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

var version = "dev"

var rootCmd = &cobra.Command{
	Use:     "dbarenactl",
	Version: version,
	Short:   "Benchmark controller for the dbarena benchmarks",
	Long: `dbarenactl drives benchctl to execute all test points for a 
provider -- sweeping tiers, running successful iterations, pulling results,
and producing files ready to submit to the dbarena/results repo.`,
	SilenceUsage: true,
}

func Execute() {
	if f, err := initLogging(); err != nil {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
		fmt.Fprintf(os.Stderr, "warning: could not open log file: %v\n", err)
	} else {
		defer f.Close() //nolint:errcheck
	}

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(resumeCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(deleteCmd)
	rootCmd.AddCommand(resultsCmd)
	rootCmd.AddCommand(pricingCmd)
}

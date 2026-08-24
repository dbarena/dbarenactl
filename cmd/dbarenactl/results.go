package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

var resultsCmd = &cobra.Command{
	Use:   "results [sweep-id]",
	Short: "Assemble result.json files from a sweep's fetched artifacts (not yet implemented)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(*cobra.Command, []string) error {
		return fmt.Errorf("dbarenactl results: not yet implemented -- run/resume/status are functional; " +
			"results assembly (medians/percentiles per results/schema/result.schema.json, warmup exclusion, " +
			"pricing lookup) is a separate follow-up")
	},
}

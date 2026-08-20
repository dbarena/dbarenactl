package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

var pricingCmd = &cobra.Command{
	Use:   "pricing",
	Short: "Fetch or manually record provider pricing snapshots (not yet implemented)",
}

var pricingFetchCmd = &cobra.Command{
	Use:   "fetch <provider>",
	Short: "Fetch pricing from a provider's machine-readable source, if one exists (not yet implemented)",
	Args:  cobra.ExactArgs(1),
	RunE: func(*cobra.Command, []string) error {
		return fmt.Errorf("dbarenactl pricing fetch: not yet implemented -- only for providers with a genuine " +
			"machine-readable, non-HTML source (e.g. AWS's Price List API); no scraping, ever")
	},
}

var pricingSetCmd = &cobra.Command{
	Use:   "set <provider>",
	Short: "Manually record a pricing snapshot for a provider with no automated source (not yet implemented)",
	Args:  cobra.ExactArgs(1),
	RunE: func(*cobra.Command, []string) error {
		return fmt.Errorf("dbarenactl pricing set: not yet implemented")
	},
}

func init() {
	pricingCmd.AddCommand(pricingFetchCmd)
	pricingCmd.AddCommand(pricingSetCmd)
}

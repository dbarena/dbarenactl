package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dbarena/dbarenactl/internal/manifest"
	"github.com/dbarena/dbarenactl/internal/pricing"
)

var (
	pricingFetchCandidate     string
	pricingSetCandidate       string
	pricingSetFile            string
	pricingSetSource          string
	pricingSetSourceUpdatedAt string
	pricingListProvider       string
	pricingListAll            bool
	pricingShowProduct        string
	pricingShowPlan           string
	pricingShowRegion         string
	pricingShowSnapshotID     string
)

var pricingCmd = &cobra.Command{
	Use:   "pricing",
	Short: "Fetch, record, and inspect provider pricing snapshots",
}

var pricingFetchCmd = &cobra.Command{
	Use:   "fetch",
	Short: "Fetch pricing from a provider's machine-readable primary source, if one exists",
	Args:  cobra.NoArgs,
	RunE:  runPricingFetch,
}

var pricingSetCmd = &cobra.Command{
	Use:   "set",
	Short: "Manually record a pricing snapshot for a provider with no automated source",
	Args:  cobra.NoArgs,
	RunE:  runPricingSet,
}

var pricingListCmd = &cobra.Command{
	Use:   "list",
	Short: "List cached pricing snapshots (latest per provider/region by default)",
	Args:  cobra.NoArgs,
	RunE:  runPricingList,
}

var pricingShowCmd = &cobra.Command{
	Use:   "show <provider>",
	Short: "Show a snapshot's line items in full",
	Args:  cobra.ExactArgs(1),
	RunE:  runPricingShow,
}

func init() {
	pricingFetchCmd.Flags().StringVar(&pricingFetchCandidate, "candidate", "",
		"Path to a candidate manifest, e.g. candidates/aws-rds-tpcc.yaml (required) -- provider and region are derived from it, never typed by hand")
	_ = pricingFetchCmd.MarkFlagRequired("candidate")

	pricingSetCmd.Flags().StringVar(&pricingSetCandidate, "candidate", "",
		"Path to a candidate manifest, e.g. candidates/supabase-tpcc.yaml (required) -- provider and region are derived from it, never typed by hand")
	pricingSetCmd.Flags().StringVar(&pricingSetFile, "file", "", "Path to a JSON file containing an array of pricing items (required)")
	pricingSetCmd.Flags().StringVar(&pricingSetSource, "source", "", "Human-readable description of where this pricing came from, e.g. a docs URL")
	pricingSetCmd.Flags().StringVar(&pricingSetSourceUpdatedAt, "source-updated-at", "",
		"When the provider itself last updated this pricing (RFC3339 or YYYY-MM-DD); omit if unknown")
	_ = pricingSetCmd.MarkFlagRequired("candidate")
	_ = pricingSetCmd.MarkFlagRequired("file")

	pricingListCmd.Flags().StringVar(&pricingListProvider, "provider", "", "Restrict to one provider")
	pricingListCmd.Flags().BoolVar(&pricingListAll, "all", false, "Show full history instead of just the latest snapshot per provider/region")

	pricingShowCmd.Flags().StringVar(&pricingShowProduct, "product", "", "Product to show, if the provider has snapshots cached for more than one")
	pricingShowCmd.Flags().StringVar(&pricingShowPlan, "plan", "", "Plan to show, if the provider has snapshots cached for more than one")
	pricingShowCmd.Flags().StringVar(&pricingShowRegion, "region", "", "Region to show, if the provider has snapshots cached for more than one")
	pricingShowCmd.Flags().StringVar(&pricingShowSnapshotID, "snapshot-id", "", "Show a specific historical snapshot instead of the latest")

	pricingCmd.AddCommand(pricingFetchCmd)
	pricingCmd.AddCommand(pricingSetCmd)
	pricingCmd.AddCommand(pricingListCmd)
	pricingCmd.AddCommand(pricingShowCmd)
}

func openPricingStore() (*pricing.Store, error) {
	dbFile, err := dbPath()
	if err != nil {
		return nil, err
	}
	return pricing.Open(dbFile)
}

func runPricingFetch(cmd *cobra.Command, _ []string) error {
	m, err := manifest.Load(pricingFetchCandidate)
	if err != nil {
		return fmt.Errorf("dbarenactl pricing fetch: load candidate manifest %q: %w", pricingFetchCandidate, err)
	}

	fetcher, ok := newPricingRegistry().Lookup(m.PricingFetcherKey())
	if !ok {
		return fmt.Errorf(
			"dbarenactl pricing fetch: no automated fetcher registered for provider %q -- "+
				"use `dbarenactl pricing set --candidate %s --file <path>` to record a snapshot manually",
			displayName(m), pricingFetchCandidate)
	}
	if m.Region == "" {
		return fmt.Errorf(
			"dbarenactl pricing fetch: candidate %q has no region: field, but provider %q needs one -- "+
				"add region: <cloud-region> to the manifest (never guessed automatically)",
			pricingFetchCandidate, displayName(m))
	}

	result, err := fetcher.Fetch(cmd.Context(), m.PricingFetcherKey(), m.Region)
	if err != nil {
		return fmt.Errorf(
			"dbarenactl pricing fetch %s: %w -- if this persists, use `dbarenactl pricing set --candidate %s --file <path>` to record a snapshot manually",
			displayName(m), err, pricingFetchCandidate)
	}

	id, err := newSnapshotID()
	if err != nil {
		return err
	}
	store, err := openPricingStore()
	if err != nil {
		return err
	}
	defer store.Close() //nolint:errcheck

	snap := &pricing.Snapshot{
		ID: id, Provider: m.Provider, Product: m.Product, Plan: m.Plan, Region: m.Region, Method: pricing.MethodFetch,
		Source: result.Source, Currency: "USD", FetchedAt: time.Now().UTC(),
		SourceUpdatedAt: result.SourceUpdatedAt, Items: result.Items,
	}
	if err := store.CreateSnapshot(snap); err != nil {
		return err
	}
	printSnapshotSummary(snap)
	return nil
}

func runPricingSet(_ *cobra.Command, _ []string) error {
	m, err := manifest.Load(pricingSetCandidate)
	if err != nil {
		return fmt.Errorf("dbarenactl pricing set: load candidate manifest %q: %w", pricingSetCandidate, err)
	}

	data, err := os.ReadFile(pricingSetFile)
	if err != nil {
		return fmt.Errorf("dbarenactl pricing set: read %s: %w", pricingSetFile, err)
	}
	var items []pricing.Item
	if err := json.Unmarshal(data, &items); err != nil {
		return fmt.Errorf("dbarenactl pricing set: parse %s: %w", pricingSetFile, err)
	}
	if err := validatePricingItems(items); err != nil {
		return fmt.Errorf("dbarenactl pricing set: %s: %w", pricingSetFile, err)
	}

	var sourceUpdatedAt *time.Time
	if pricingSetSourceUpdatedAt != "" {
		t, err := parseFlexibleDate(pricingSetSourceUpdatedAt)
		if err != nil {
			return fmt.Errorf("dbarenactl pricing set: --source-updated-at: %w", err)
		}
		sourceUpdatedAt = &t
	}

	id, err := newSnapshotID()
	if err != nil {
		return err
	}
	store, err := openPricingStore()
	if err != nil {
		return err
	}
	defer store.Close() //nolint:errcheck

	snap := &pricing.Snapshot{
		ID: id, Provider: m.Provider, Product: m.Product, Plan: m.Plan, Region: m.Region, Method: pricing.MethodSet,
		Source: pricingSetSource, Currency: "USD", FetchedAt: time.Now().UTC(),
		SourceUpdatedAt: sourceUpdatedAt, Items: items,
	}
	if err := store.CreateSnapshot(snap); err != nil {
		return err
	}
	printSnapshotSummary(snap)
	return nil
}

func validatePricingItems(items []pricing.Item) error {
	if len(items) == 0 {
		return fmt.Errorf("file must contain at least one item")
	}
	for i, it := range items {
		if it.Description == "" {
			return fmt.Errorf("item %d: description is required", i)
		}
		if it.Unit == "" {
			return fmt.Errorf("item %d: unit is required", i)
		}
		if it.PriceUSD < 0 {
			return fmt.Errorf("item %d: price_usd must not be negative", i)
		}
	}
	return nil
}

// parseFlexibleDate accepts RFC3339 or a bare YYYY-MM-DD date (interpreted
// as UTC midnight), covering both "I know the exact instant" and "I only
// know the day this changed" operator inputs.
func parseFlexibleDate(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("%q is not RFC3339 or YYYY-MM-DD", s)
}

func runPricingList(_ *cobra.Command, _ []string) error {
	store, err := openPricingStore()
	if err != nil {
		return err
	}
	defer store.Close() //nolint:errcheck

	var snaps []*pricing.Snapshot
	if pricingListAll {
		snaps, err = store.ListAllSnapshots(pricingListProvider)
	} else {
		snaps, err = store.ListLatestSnapshots(pricingListProvider)
	}
	if err != nil {
		return err
	}
	if len(snaps) == 0 {
		fmt.Println("No pricing snapshots recorded yet.")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tPRODUCT\tPLAN\tREGION\tMETHOD\tFETCHED AT (UTC)\tSOURCE UPDATED (UTC)\tITEMS\tSNAPSHOT ID")
	for _, s := range snaps {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			s.Provider, s.Product, dashIfEmpty(s.Plan), dashIfEmpty(s.Region), s.Method,
			s.FetchedAt.Format("2006-01-02 15:04"), sourceUpdatedAtOrUnknown(s.SourceUpdatedAt),
			len(s.Items), s.ID)
	}
	return tw.Flush()
}

func runPricingShow(_ *cobra.Command, args []string) error {
	provider := args[0]
	store, err := openPricingStore()
	if err != nil {
		return err
	}
	defer store.Close() //nolint:errcheck

	snap, err := resolveShowSnapshot(store, provider)
	if err != nil {
		return err
	}

	printSnapshotSummary(snap)
	fmt.Println()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SKU\tDESCRIPTION\tUNIT\tPRICE (USD)\tREGION\tATTRIBUTES")
	for _, it := range snap.Items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.6f\t%s\t%s\n",
			dashIfEmpty(it.SKU), it.Description, it.Unit, it.PriceUSD, dashIfEmpty(it.Region), formatAttributes(it.Attributes))
	}
	return tw.Flush()
}

func resolveShowSnapshot(store *pricing.Store, provider string) (*pricing.Snapshot, error) {
	if pricingShowSnapshotID != "" {
		snap, err := store.GetSnapshot(pricingShowSnapshotID)
		if err != nil {
			return nil, fmt.Errorf("dbarenactl pricing show %s: %w", provider, err)
		}
		if snap.Provider != provider {
			return nil, fmt.Errorf("dbarenactl pricing show: snapshot %s belongs to provider %q, not %q", pricingShowSnapshotID, snap.Provider, provider)
		}
		return snap, nil
	}

	latest, err := store.ListLatestSnapshots(provider)
	if err != nil {
		return nil, fmt.Errorf("dbarenactl pricing show %s: %w", provider, err)
	}
	if pricingShowProduct != "" {
		latest = filterSnapshots(latest, func(s *pricing.Snapshot) bool { return s.Product == pricingShowProduct })
	}
	if pricingShowPlan != "" {
		latest = filterSnapshots(latest, func(s *pricing.Snapshot) bool { return s.Plan == pricingShowPlan })
	}
	if pricingShowRegion != "" {
		latest = filterSnapshots(latest, func(s *pricing.Snapshot) bool { return s.Region == pricingShowRegion })
	}

	switch len(latest) {
	case 0:
		return nil, fmt.Errorf("dbarenactl pricing show %s: %w", provider, pricing.ErrNotFound)
	case 1:
		return latest[0], nil
	default:
		combos := make([]string, len(latest))
		for i, s := range latest {
			combos[i] = fmt.Sprintf("product=%s plan=%s region=%s", s.Product, dashIfEmpty(s.Plan), dashIfEmpty(s.Region))
		}
		sort.Strings(combos)
		return nil, fmt.Errorf("dbarenactl pricing show %s: multiple snapshots cached (%s) -- specify --product/--plan/--region to disambiguate",
			provider, strings.Join(combos, "; "))
	}
}

func filterSnapshots(snaps []*pricing.Snapshot, keep func(*pricing.Snapshot) bool) []*pricing.Snapshot {
	var out []*pricing.Snapshot
	for _, s := range snaps {
		if keep(s) {
			out = append(out, s)
		}
	}
	return out
}

// displayName renders a manifest's provider identity for error/summary
// messages, e.g. "GCP" or "GCP (Enterprise Plus)".
func displayName(m *manifest.Manifest) string {
	if m.Plan != "" {
		return m.Provider + " (" + m.Plan + ")"
	}
	return m.Provider
}

func printSnapshotSummary(s *pricing.Snapshot) {
	fmt.Printf("Provider:          %s\n", s.Provider)
	fmt.Printf("Product:           %s\n", s.Product)
	fmt.Printf("Plan:              %s\n", dashIfEmpty(s.Plan))
	fmt.Printf("Region:            %s\n", dashIfEmpty(s.Region))
	fmt.Printf("Method:            %s\n", s.Method)
	fmt.Printf("Source:            %s\n", dashIfEmpty(s.Source))
	fmt.Printf("Fetched at:        %s\n", s.FetchedAt.Format(time.RFC3339))
	fmt.Printf("Source updated at: %s\n", sourceUpdatedAtOrUnknown(s.SourceUpdatedAt))
	fmt.Printf("Items:             %d\n", len(s.Items))
	fmt.Printf("Snapshot ID:       %s\n", s.ID)
}

func sourceUpdatedAtOrUnknown(t *time.Time) string {
	if t == nil {
		return "unknown"
	}
	return t.Format(time.RFC3339)
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func formatAttributes(attrs map[string]string) string {
	if len(attrs) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + attrs[k]
	}
	return strings.Join(parts, ",")
}

// newSnapshotID returns a random 128-bit hex identifier for a new snapshot
// row. Unlike sweep/run/test-point ids elsewhere in this codebase, snapshot
// ids don't need to be deterministic -- they just need to be unique per
// insert -- so a plain random id avoids pulling in a UUID dependency this
// repo doesn't otherwise use.
func newSnapshotID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("dbarenactl pricing: generate snapshot id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

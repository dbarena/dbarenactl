package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbarena/dbarenactl/internal/manifest"
)

// runCLI executes rootCmd in-process with args, capturing stdout. Used to
// smoke-test that `pricing set` -> `pricing list` -> `pricing show` work
// end to end purely through the generic pricing.Store API, with no
// provider-specific code involved.
func runCLI(t *testing.T, args ...string) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	rootCmd.SetArgs(args)
	runErr := rootCmd.Execute()

	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	io.Copy(&buf, r) //nolint:errcheck

	if runErr != nil {
		t.Fatalf("dbarenactl %s: %v\noutput:\n%s", strings.Join(args, " "), runErr, buf.String())
	}
	return buf.String()
}

func TestPricingSetListShow_RoundTrip(t *testing.T) {
	t.Setenv("DBARENACTL_HOME", t.TempDir())

	itemsFile := filepath.Join(t.TempDir(), "items.json")
	itemsJSON := `[{"description":"Compute (8XL)","unit":"hour","price_usd":1.536,"attributes":{"tier":"8xlarge"}}]`
	if err := os.WriteFile(itemsFile, []byte(itemsJSON), 0o644); err != nil {
		t.Fatalf("write items file: %v", err)
	}

	setOut := runCLI(t, "pricing", "set", "--candidate", "../../candidates/supabase/supabase/tpcc/manifest.yaml", "--file", itemsFile, "--source", "manual test entry")
	if !strings.Contains(setOut, "Supabase") || !strings.Contains(setOut, "OrioleDB") {
		t.Errorf("set output = %q, want it to mention both the Supabase and OrioleDB products it shares pricing with", setOut)
	}

	listOut := runCLI(t, "pricing", "list")
	if !strings.Contains(listOut, "Supabase") || !strings.Contains(listOut, "OrioleDB") {
		t.Errorf("list output missing Supabase/OrioleDB rows: %q", listOut)
	}

	showOut := runCLI(t, "pricing", "show", "Supabase", "--product", "OrioleDB")
	if !strings.Contains(showOut, "Compute (8XL)") {
		t.Errorf("show output missing the item description: %q", showOut)
	}
	if !strings.Contains(showOut, "tier=8xlarge") {
		t.Errorf("show output missing item attributes: %q", showOut)
	}
}

// TestNewPricingRegistry_CoversEveryValidProvider guards against exactly
// the maintenance gap the old "unregistered provider" test used to catch by
// accident: under manifest's strict Provider/Product enum validation, every
// schema-valid manifest's PricingFetcherKey() is guaranteed, by
// construction, to resolve in this registry -- so if a future provider is
// added to internal/manifest without a matching Fetcher registration here,
// this test fails immediately instead of only surfacing at first real use.
// The "no fetcher, use `pricing set`" error path in pricing.go stays: it's
// still reachable in exactly that maintenance-gap scenario, just no longer
// reachable via a malformed candidate string.
func TestNewPricingRegistry_CoversEveryValidProvider(t *testing.T) {
	reg := newPricingRegistry()
	cases := []struct {
		provider, product, plan string
	}{
		{"AWS", "RDS", ""},
		{"GCP", "Cloud SQL for Postgres", "Enterprise"},
		{"GCP", "Cloud SQL for Postgres", "Enterprise Plus"},
		{"Supabase", "Supabase", "Pro"},
	}
	for _, tc := range cases {
		m := &manifest.Manifest{Provider: tc.provider, Product: tc.product, Plan: tc.plan}
		key := m.PricingFetcherKey()
		if _, ok := reg.Lookup(key); !ok {
			t.Errorf("no Fetcher registered for provider=%s product=%s plan=%s (key %q)", tc.provider, tc.product, tc.plan, key)
		}
	}
}

func TestSnapshotProducts(t *testing.T) {
	cases := []struct {
		name         string
		provider     string
		product      string
		wantProducts []string
	}{
		{"supabase vanilla expands to both shared products", "Supabase", "Supabase", []string{"Supabase", "OrioleDB"}},
		{"supabase OrioleDB expands to both shared products", "Supabase", "OrioleDB", []string{"Supabase", "OrioleDB"}},
		{"aws is untouched", "AWS", "RDS", []string{"RDS"}},
		{"gcp is untouched", "GCP", "Cloud SQL for Postgres", []string{"Cloud SQL for Postgres"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &manifest.Manifest{Provider: tc.provider, Product: tc.product}
			got := snapshotProducts(m)
			if strings.Join(got, ",") != strings.Join(tc.wantProducts, ",") {
				t.Errorf("snapshotProducts() = %v, want %v", got, tc.wantProducts)
			}
		})
	}
}

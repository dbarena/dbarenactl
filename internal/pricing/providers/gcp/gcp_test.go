package gcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/services", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("pageToken") == "" {
			writeJSONFile(t, w, "testdata/services_page1.json")
			return
		}
		writeJSONFile(t, w, "testdata/services_page2.json")
	})
	mux.HandleFunc("/services/9662-B51E-5089/skus", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("pageToken") == "" {
			writeJSONFile(t, w, "testdata/skus_page1.json")
			return
		}
		writeJSONFile(t, w, "testdata/skus_page2.json")
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func writeJSONFile(t *testing.T, w http.ResponseWriter, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func TestFetch_FiltersAndExtractsPrices(t *testing.T) {
	srv := newTestServer(t)
	f := &Fetcher{
		BaseURL:     srv.URL,
		AccessToken: func(context.Context) (string, error) { return "test-token", nil },
	}

	result, err := f.Fetch(context.Background(), "gcp/cloudsql", "us-east1")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if len(result.Items) != 2 {
		t.Fatalf("got %d items, want 2 (postgres+us-east1 only, decoy/regional/wrong-region/mysql excluded): %+v", len(result.Items), result.Items)
	}
	byDesc := map[string]float64{}
	for _, it := range result.Items {
		byDesc[it.Description] = it.PriceUSD
	}
	if price := byDesc["Cloud SQL for PostgreSQL: vCPU time in Americas"]; price != 0.0537 {
		t.Errorf("vCPU price = %v, want 0.0537", price)
	}
	if price := byDesc["Cloud SQL for PostgreSQL: storage PD SSD in Americas"]; price != 0.091 {
		t.Errorf("storage price = %v, want 0.091", price)
	}

	if !strings.Contains(result.Source, "9662-B51E-5089") || !strings.Contains(result.Source, "us-east1") {
		t.Errorf("Source = %q, want it to mention the resolved service id and region", result.Source)
	}

	wantUpdated := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	if result.SourceUpdatedAt == nil || !result.SourceUpdatedAt.Equal(wantUpdated) {
		t.Errorf("SourceUpdatedAt = %v, want max effectiveTime across selected SKUs %v", result.SourceUpdatedAt, wantUpdated)
	}
}

func TestFetch_RequiresRegion(t *testing.T) {
	f := &Fetcher{AccessToken: func(context.Context) (string, error) { return "t", nil }}
	if _, err := f.Fetch(context.Background(), "gcp/cloudsql", ""); err == nil {
		t.Error("expected an error when region is empty")
	}
}

func TestFilterSKUs(t *testing.T) {
	skus := []sku{
		{Description: "Cloud SQL for PostgreSQL: vCPU time in Americas", ServiceRegions: []string{"us-east1"}},
		{Description: "Cloud SQL for PostgreSQL: Regional storage in Americas", ServiceRegions: []string{"us-east1"}},
		{Description: "Cloud SQL for PostgreSQL: vCPU time in Europe", ServiceRegions: []string{"europe-west1"}},
		{Description: "Cloud SQL for MySQL: vCPU time in Americas", ServiceRegions: []string{"us-east1"}},
	}
	got := filterSKUs(skus, "us-east1")
	if len(got) != 1 || got[0].Description != "Cloud SQL for PostgreSQL: vCPU time in Americas" {
		t.Errorf("filterSKUs = %+v, want only the plain zonal us-east1 postgres SKU", got)
	}
}

func TestPriceOf_UsesFirstTierAndStringUnits(t *testing.T) {
	s := sku{PricingInfo: []pricingInfo{{
		PricingExpression: pricingExpression{
			TieredRates: []tieredRate{
				{UnitPrice: money{Units: "0", Nanos: 53700000}},
				{UnitPrice: money{Units: "1", Nanos: 0}}, // must NOT be picked -- [0] is the on-demand rate
			},
		},
	}}}
	price, err := priceOf(s)
	if err != nil {
		t.Fatalf("priceOf: %v", err)
	}
	if price != 0.0537 {
		t.Errorf("price = %v, want 0.0537 (from tieredRates[0], not [1])", price)
	}
}

func TestGCloudAccessToken_UnusableBinary(t *testing.T) {
	emptyDir := t.TempDir()
	t.Setenv("PATH", emptyDir)
	_, err := gcloudAccessToken(context.Background())
	if !errors.Is(err, ErrGCloudUnusable) {
		t.Errorf("err = %v, want ErrGCloudUnusable", err)
	}
}

func TestGCloudAccessToken_AuthError(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "gcloud")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'no active credentials' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake gcloud: %v", err)
	}
	t.Setenv("PATH", dir)
	_, err := gcloudAccessToken(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrGCloudUnusable) {
		t.Errorf("err = %v, should NOT be ErrGCloudUnusable -- gcloud ran, it just has no credentials", err)
	}
	if !strings.Contains(err.Error(), "gcloud auth login") {
		t.Errorf("err = %v, want it to mention `gcloud auth login`", err)
	}
}

package supabase

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func newTestServer(t *testing.T, fixture string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		data, err := os.ReadFile(fixture)
		if err != nil {
			t.Fatalf("read fixture %s: %v", fixture, err)
		}
		w.Header().Set("Content-Type", "text/markdown")
		w.Write(data)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetch_RealFixture(t *testing.T) {
	srv := newTestServer(t, "testdata/pricing.md")
	f := &Fetcher{PricingMDURL: srv.URL}

	result, err := f.Fetch(context.Background(), "supabase", "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	var computeCount, diskCount, planCount int
	byDiskType := map[string]int{}
	for _, it := range result.Items {
		switch {
		case it.SKU == "plan-pro":
			planCount++
			if it.PriceUSD != 25 {
				t.Errorf("Pro plan price = %v, want 25 (extracted, not hardcoded)", it.PriceUSD)
			}
			if it.Attributes["included_compute_credit_usd"] != "10" {
				t.Errorf("Pro plan credit attrs = %+v, want included_compute_credit_usd=10", it.Attributes)
			}
			if it.Attributes["included_compute_credit_covers"] != "Micro" {
				t.Errorf("Pro plan credit attrs = %+v, want included_compute_credit_covers=Micro", it.Attributes)
			}
		case strings.HasPrefix(it.SKU, "disk-"):
			diskCount++
			dt := it.Attributes["disk_type"]
			if dt != "gp3" && dt != "io2" {
				t.Errorf("disk item %+v: disk_type = %q, want gp3 or io2", it, dt)
			}
			byDiskType[dt]++
		default:
			computeCount++
			if it.Attributes["billing_granularity"] != "hourly" {
				t.Errorf("compute item %+v missing billing_granularity=hourly", it)
			}
		}
	}

	if computeCount != 10 {
		t.Errorf("compute items = %d, want 10 (Micro..16XL, Contact Us row skipped)", computeCount)
	}
	if diskCount != 5 {
		t.Errorf("disk items = %d, want 5 (3 gp3 + 2 io2)", diskCount)
	}
	if byDiskType["gp3"] != 3 || byDiskType["io2"] != 2 {
		t.Errorf("byDiskType = %+v, want gp3=3 io2=2", byDiskType)
	}
	if planCount != 1 {
		t.Errorf("plan items = %d, want 1", planCount)
	}

	if result.SourceUpdatedAt != nil {
		t.Errorf("SourceUpdatedAt = %v, want nil -- pricing.md has no freshness signal", result.SourceUpdatedAt)
	}
	if !strings.Contains(result.Source, "Supabase pricing") || !strings.Contains(result.Source, "fetched live") {
		t.Errorf("Source = %q, want it to describe Supabase pricing fetched live", result.Source)
	}
}

func TestParseCompute_MissingSectionErrors(t *testing.T) {
	if _, _, _, err := parseCompute("no compute section here"); err == nil {
		t.Error("expected an error when the Compute Add-Ons section is missing")
	}
}

func TestParseDisk_MissingSectionErrors(t *testing.T) {
	if _, err := parseDisk("no disk section here"); err == nil {
		t.Error("expected an error when the Disk Storage section is missing")
	}
}

func TestParseDisk_HighPerformanceThroughputDriftErrors(t *testing.T) {
	doc := `## Disk Storage
### General Purpose
- Size: 8 GB included, then $0.125 per GB
- IOPS: 3,000 IOPS included, then $0.024 per IOPS
- Throughput: 125 MB/s included, then $0.095 per MB/s
### High Performance
- Size: $0.195 per GB
- IOPS: $0.119 per IOPS
- Throughput: Now costs $0.01 per MB/s, surprise!
## Add-Ons
`
	if _, err := parseDisk(doc); err == nil {
		t.Error("expected an error when the High Performance throughput line no longer matches the known literal")
	}
}

func TestParsePro_MissingHeadingErrors(t *testing.T) {
	if _, err := parsePro("no pro heading here", "10", "Micro"); err == nil {
		t.Error("expected an error when the Pro heading is missing")
	}
}

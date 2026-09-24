package aws

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/offers/v1.0/aws/AmazonRDS/current/us-east-1/index.json", func(w http.ResponseWriter, r *http.Request) {
		data, err := os.ReadFile("testdata/offer_file.json")
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetch_FiltersToPostgresSingleAZ(t *testing.T) {
	srv := newTestServer(t)
	f := &Fetcher{BaseURL: srv.URL}

	result, err := f.Fetch(context.Background(), "aws/rds", "us-east-1")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if len(result.Items) != 7 {
		t.Fatalf("got %d items, want 7 (instance+storage-gp3+storage-gp2+iops-gp3+iops-io1+throughput+cpu-credits; mysql/multi-az/licensed excluded): %+v", len(result.Items), result.Items)
	}

	excludedFromByUnit := map[string]bool{"SKU_STORAGE_GP2": true, "SKU_IOPS_IO1": true}
	byUnit := map[string]float64{}
	for _, it := range result.Items {
		if !excludedFromByUnit[it.SKU] {
			byUnit[it.Unit] = it.PriceUSD
		}
		if it.Region != "us-east-1" {
			t.Errorf("item %+v: Region = %q, want us-east-1", it, it.Region)
		}
	}
	want := map[string]float64{"Hrs": 0.034, "GB-Mo": 0.115, "IOPS-Mo": 0.02, "MiBps-Mo": 0.08, "vCPU-Hours": 0.075}
	for unit, price := range want {
		if got := byUnit[unit]; got != price {
			t.Errorf("unit %s: price = %v, want %v", unit, got, price)
		}
	}

	for _, it := range result.Items {
		switch it.SKU {
		case "SKU_INSTANCE":
			if it.Attributes["db_instance_type"] != "db.t4g.small" {
				t.Errorf("instance item Attributes = %+v, want db_instance_type=db.t4g.small (not the raw instanceType key)", it.Attributes)
			}
			if _, present := it.Attributes["instanceType"]; present {
				t.Errorf("instance item Attributes = %+v, want no raw camelCase instanceType key", it.Attributes)
			}
		case "SKU_STORAGE":
			if it.Attributes["disk_type"] != "gp3" {
				t.Errorf("storage item Attributes = %+v, want disk_type=gp3 (normalized from AWS's raw usagetype %q)", it.Attributes, "RDS:GP3-Storage")
			}
			if _, present := it.Attributes["volumeType"]; present {
				t.Errorf("storage item Attributes = %+v, want no raw volumeType key", it.Attributes)
			}
		case "SKU_STORAGE_GP2":
			// Regression check: AWS's raw usagetype for gp2 ("RDS:GP2-Storage")
			// is not a substring of gp3's ("RDS:GP3-Storage"), so the two must
			// not collapse onto the same disk_type.
			if it.Attributes["disk_type"] != "gp2" {
				t.Errorf("gp2 storage item Attributes = %+v, want disk_type=gp2, not collapsed onto gp3", it.Attributes)
			}
		case "SKU_IOPS":
			if it.Attributes["disk_type"] != "gp3" {
				t.Errorf("gp3 IOPS item Attributes = %+v, want disk_type=gp3", it.Attributes)
			}
		case "SKU_IOPS_IO1":
			// Regression check: real AWS omits the volumeType attribute
			// entirely on this SKU (unlike Database Storage products), so
			// disk_type must come from usagetype ("RDS:PIOPS") instead, and
			// must not collapse onto the gp3 IOPS SKU above.
			if it.Attributes["disk_type"] != "io1" {
				t.Errorf("io1 IOPS item Attributes = %+v, want disk_type=io1, not collapsed onto gp3", it.Attributes)
			}
		case "SKU_CPU_CREDITS":
			// Regression check: this product family carries neither
			// deploymentOption nor licenseModel, unlike every other family
			// extractItems keeps -- it must still survive the filter.
			if it.Attributes["instance_family"] != "T4G" {
				t.Errorf("CPU credits item Attributes = %+v, want instance_family=T4G", it.Attributes)
			}
		}
		if it.Attributes["database_engine"] != "PostgreSQL" {
			t.Errorf("item %+v: database_engine should be snake_case and set", it)
		}
	}

	wantUpdated := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	if result.SourceUpdatedAt == nil || !result.SourceUpdatedAt.Equal(wantUpdated) {
		t.Errorf("SourceUpdatedAt = %v, want offer file's publicationDate %v", result.SourceUpdatedAt, wantUpdated)
	}

	if !strings.Contains(result.Source, "us-east-1") {
		t.Errorf("Source = %q, want it to mention the region", result.Source)
	}
}

func TestDiskTypeFromUsageType(t *testing.T) {
	cases := []struct{ usageType, want string }{
		{"RDS:GP2-Storage", "gp2"},
		{"RDS:GP3-Storage", "gp3"},
		{"RDS:PIOPS-Storage", "io1"},
		{"RDS:PIOPS-Storage-IO2", "io2"},
		{"RDS:PIOPS", "io1"},
		{"RDS:GP3-PIOPS", "gp3"},
		{"RDS:IO2-PIOPS", "io2"},
		{"RDS:GP3-Throughput", "gp3"},
	}
	for _, c := range cases {
		if got := diskTypeFromUsageType(c.usageType); got != c.want {
			t.Errorf("diskTypeFromUsageType(%q) = %q, want %q", c.usageType, got, c.want)
		}
	}
}

func TestFetch_RequiresRegion(t *testing.T) {
	f := &Fetcher{}
	if _, err := f.Fetch(context.Background(), "aws/rds", ""); err == nil {
		t.Error("expected an error when region is empty")
	}
}

func TestFetch_NoMatchesIsAnError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/offers/v1.0/aws/AmazonRDS/current/eu-west-9/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"publicationDate":"2026-01-01T00:00:00Z","products":{},"terms":{"OnDemand":{}}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	f := &Fetcher{BaseURL: srv.URL}
	if _, err := f.Fetch(context.Background(), "aws/rds", "eu-west-9"); err == nil {
		t.Error("expected an error when no SKUs match")
	}
}

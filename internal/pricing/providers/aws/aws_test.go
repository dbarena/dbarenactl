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

	if len(result.Items) != 4 {
		t.Fatalf("got %d items, want 4 (instance+storage+iops+throughput; mysql/multi-az/licensed excluded): %+v", len(result.Items), result.Items)
	}

	byUnit := map[string]float64{}
	for _, it := range result.Items {
		byUnit[it.Unit] = it.PriceUSD
		if it.Region != "us-east-1" {
			t.Errorf("item %+v: Region = %q, want us-east-1", it, it.Region)
		}
	}
	want := map[string]float64{"Hrs": 0.034, "GB-Mo": 0.115, "IOPS-Mo": 0.02, "MiBps-Mo": 0.08}
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
				t.Errorf("storage item Attributes = %+v, want disk_type=gp3 (normalized from AWS's raw volumeType)", it.Attributes)
			}
			if _, present := it.Attributes["volumeType"]; present {
				t.Errorf("storage item Attributes = %+v, want no raw volumeType key", it.Attributes)
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

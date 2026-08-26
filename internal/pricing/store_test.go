package pricing

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "dbarenactl.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func seedSnapshot(t *testing.T, st *Store, provider, product, plan, region string, fetchedAt time.Time, items []Item) *Snapshot {
	t.Helper()
	snap := &Snapshot{
		ID:       provider + "|" + product + "|" + plan + "|" + region + "|" + fetchedAt.Format(time.RFC3339Nano),
		Provider: provider, Product: product, Plan: plan, Region: region, Method: MethodFetch, Source: "test", Currency: "USD",
		FetchedAt: fetchedAt, Items: items,
	}
	if err := st.CreateSnapshot(snap); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	return snap
}

func TestSnapshot_CreateAndGet(t *testing.T) {
	st := openTestStore(t)
	updated := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	snap := &Snapshot{
		ID: "snap-1", Provider: "AWS", Product: "RDS", Plan: "", Region: "us-east-1", Method: MethodFetch,
		Source: "AWS RDS Price List API", Currency: "USD", FetchedAt: time.Now().UTC(),
		SourceUpdatedAt: &updated,
		Items: []Item{{SKU: "sku-1", Description: "db.t4g.small", Unit: "hour", PriceUSD: 0.034,
			Attributes: map[string]string{"db_instance_type": "db.t4g.small"}}},
	}
	if err := st.CreateSnapshot(snap); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	got, err := st.GetSnapshot("snap-1")
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if got.Provider != "AWS" || got.Product != "RDS" || got.Plan != "" || got.Region != "us-east-1" || got.Method != MethodFetch {
		t.Errorf("got = %+v", got)
	}
	if got.SourceUpdatedAt == nil || !got.SourceUpdatedAt.Equal(updated) {
		t.Errorf("SourceUpdatedAt = %v, want %v", got.SourceUpdatedAt, updated)
	}
	if len(got.Items) != 1 || got.Items[0].SKU != "sku-1" || got.Items[0].Attributes["db_instance_type"] != "db.t4g.small" {
		t.Errorf("Items round-trip failed: %+v", got.Items)
	}
}

func TestSnapshot_GetNotFound(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.GetSnapshot("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if _, err := st.GetLatestSnapshot("AWS", "RDS", "", "us-east-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestGetLatestSnapshot_ScopedByFullKey(t *testing.T) {
	st := openTestStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	seedSnapshot(t, st, "GCP", "Cloud SQL for Postgres", "Enterprise", "us-east1", base, []Item{{Description: "old-enterprise", Unit: "hour", PriceUSD: 1}})
	newEnterprise := seedSnapshot(t, st, "GCP", "Cloud SQL for Postgres", "Enterprise", "us-east1", base.Add(time.Hour), []Item{{Description: "new-enterprise", Unit: "hour", PriceUSD: 2}})
	enterprisePlus := seedSnapshot(t, st, "GCP", "Cloud SQL for Postgres", "Enterprise Plus", "us-east1", base.Add(2*time.Hour), []Item{{Description: "enterprise-plus", Unit: "hour", PriceUSD: 3}})
	seedSnapshot(t, st, "AWS", "RDS", "", "us-east1", base.Add(3*time.Hour), []Item{{Description: "aws-should-not-leak", Unit: "hour", PriceUSD: 4}})

	got, err := st.GetLatestSnapshot("GCP", "Cloud SQL for Postgres", "Enterprise", "us-east1")
	if err != nil {
		t.Fatalf("GetLatestSnapshot: %v", err)
	}
	if got.ID != newEnterprise.ID {
		t.Errorf("got %s, want newest Enterprise snapshot %s -- Enterprise Plus must not shadow Enterprise as \"latest\"", got.ID, newEnterprise.ID)
	}

	gotPlus, err := st.GetLatestSnapshot("GCP", "Cloud SQL for Postgres", "Enterprise Plus", "us-east1")
	if err != nil {
		t.Fatalf("GetLatestSnapshot(Enterprise Plus): %v", err)
	}
	if gotPlus.ID != enterprisePlus.ID {
		t.Errorf("got %s, want %s", gotPlus.ID, enterprisePlus.ID)
	}
}

func TestListLatestSnapshots_OnePerFullKey(t *testing.T) {
	st := openTestStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	seedSnapshot(t, st, "GCP", "Cloud SQL for Postgres", "Enterprise", "us-east1", base, nil)
	wantEnterprise := seedSnapshot(t, st, "GCP", "Cloud SQL for Postgres", "Enterprise", "us-east1", base.Add(time.Hour), nil)
	wantEnterprisePlus := seedSnapshot(t, st, "GCP", "Cloud SQL for Postgres", "Enterprise Plus", "us-east1", base, nil)
	seedSnapshot(t, st, "AWS", "RDS", "", "us-east-1", base, nil)
	wantAWS := seedSnapshot(t, st, "AWS", "RDS", "", "us-east-1", base.Add(time.Hour), nil)

	all, err := st.ListLatestSnapshots("")
	if err != nil {
		t.Fatalf("ListLatestSnapshots: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d snapshots, want 3 (one per distinct provider+product+plan+region): %+v", len(all), all)
	}
	ids := map[string]bool{}
	for _, s := range all {
		ids[s.ID] = true
	}
	for _, want := range []*Snapshot{wantEnterprise, wantEnterprisePlus, wantAWS} {
		if !ids[want.ID] {
			t.Errorf("missing expected latest snapshot %s (provider=%s product=%s plan=%s)", want.ID, want.Provider, want.Product, want.Plan)
		}
	}

	gcpOnly, err := st.ListLatestSnapshots("GCP")
	if err != nil {
		t.Fatalf("ListLatestSnapshots(GCP): %v", err)
	}
	if len(gcpOnly) != 2 {
		t.Fatalf("got %d, want 2 GCP snapshots (one per plan): %+v", len(gcpOnly), gcpOnly)
	}
}

func TestListSnapshots_FullHistoryOneProvider(t *testing.T) {
	st := openTestStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedSnapshot(t, st, "GCP", "Cloud SQL for Postgres", "Enterprise", "us-east1", base, nil)
	seedSnapshot(t, st, "GCP", "Cloud SQL for Postgres", "Enterprise Plus", "us-east1", base.Add(time.Hour), nil)
	seedSnapshot(t, st, "AWS", "RDS", "", "us-east-1", base, nil)

	got, err := st.ListSnapshots("GCP")
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (full history across both GCP plans, no AWS): %+v", len(got), got)
	}
	if got[0].FetchedAt.Before(got[1].FetchedAt) {
		t.Errorf("expected newest first: %+v", got)
	}
}

func TestListAllSnapshots(t *testing.T) {
	st := openTestStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedSnapshot(t, st, "GCP", "Cloud SQL for Postgres", "Enterprise", "us-east1", base, nil)
	seedSnapshot(t, st, "GCP", "Cloud SQL for Postgres", "Enterprise", "us-east1", base.Add(time.Hour), nil)
	seedSnapshot(t, st, "AWS", "RDS", "", "us-east-1", base, nil)

	all, err := st.ListAllSnapshots("")
	if err != nil {
		t.Fatalf("ListAllSnapshots: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d, want 3 (full history, no dedup, all providers): %+v", len(all), all)
	}

	gcpOnly, err := st.ListAllSnapshots("GCP")
	if err != nil {
		t.Fatalf("ListAllSnapshots(GCP): %v", err)
	}
	if len(gcpOnly) != 2 {
		t.Fatalf("got %d, want 2: %+v", len(gcpOnly), gcpOnly)
	}
}

func TestCreateSnapshot_RepeatedFetchAppendsNotOverwrites(t *testing.T) {
	st := openTestStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	items := []Item{{Description: "unchanged", Unit: "hour", PriceUSD: 1}}
	seedSnapshot(t, st, "AWS", "RDS", "", "us-east-1", base, items)
	seedSnapshot(t, st, "AWS", "RDS", "", "us-east-1", base.Add(time.Hour), items)

	history, err := st.ListSnapshots("AWS")
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("got %d snapshots, want 2 -- an unchanged re-fetch must still append a new row, not overwrite", len(history))
	}
}

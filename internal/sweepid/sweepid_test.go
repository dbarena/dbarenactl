package sweepid

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

func TestCompute_Deterministic(t *testing.T) {
	p := Params{
		Provider:        "AWS",
		Product:         "RDS",
		ManifestContent: []byte("test-points: []\n"),
		MaxConcurrency:  6,
		Iterations:      5,
	}
	a := Compute(p)
	b := Compute(p)
	if a != b {
		t.Errorf("Compute is not deterministic: %q != %q", a, b)
	}
}

func TestCompute_ExamplePrefixes(t *testing.T) {
	cases := []struct {
		name       string
		p          Params
		wantPrefix string
	}{
		{"aws, no plan", Params{Provider: "AWS", Product: "RDS", Workload: "tpcc"}, "aws-tpcc-"},
		{"gcp, Enterprise", Params{Provider: "GCP", Product: "Cloud SQL for Postgres", Plan: "Enterprise", Workload: "tpcc"}, "gcp-enterprise-tpcc-"},
		{"gcp, Enterprise Plus", Params{Provider: "GCP", Product: "Cloud SQL for Postgres", Plan: "Enterprise Plus", Workload: "tpcc"}, "gcp-enterprise-plus-tpcc-"},
		{"supabase, Pro", Params{Provider: "Supabase", Product: "Supabase", Plan: "Pro", Workload: "tpcc"}, "supabase-pro-tpcc-"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := Compute(tc.p)
			if !strings.HasPrefix(id, tc.wantPrefix) {
				t.Errorf("id = %q, want prefix %q", id, tc.wantPrefix)
			}
			if len(id) != len(tc.wantPrefix)+12 {
				t.Errorf("id = %q, want a 12-char hash after the prefix", id)
			}
		})
	}
}

func TestCompute_DifferentWorkloadDifferentPrefix(t *testing.T) {
	tpcc := Compute(Params{Provider: "AWS", Product: "RDS", Workload: "tpcc"})
	tpce := Compute(Params{Provider: "AWS", Product: "RDS", Workload: "tpce"})
	if tpcc == tpce {
		t.Fatal("different workloads should never collide")
	}
	if strings.HasPrefix(tpcc, "aws-tpce-") || strings.HasPrefix(tpce, "aws-tpcc-") {
		t.Errorf("ids should be visually distinguishable by workload, got %q and %q", tpcc, tpce)
	}
}

func TestCompute_ProductChangesIdEvenThoughNotInPrefix(t *testing.T) {
	rds := Compute(Params{Provider: "AWS", Product: "RDS", Workload: "tpcc"})
	aurora := Compute(Params{Provider: "AWS", Product: "Aurora", Workload: "tpcc"})
	if rds == aurora {
		t.Fatal("a different Product must produce a different id, even though Product isn't shown in the prefix")
	}
	if !strings.HasPrefix(rds, "aws-tpcc-") || !strings.HasPrefix(aurora, "aws-tpcc-") {
		t.Errorf("both should share the same visible prefix (Product is hashed but not displayed): rds=%q aurora=%q", rds, aurora)
	}
}

func TestCompute_DiffersOnAnyParamChange(t *testing.T) {
	base := Params{
		Provider:            "AWS",
		Product:             "RDS",
		Workload:            "tpcc",
		ManifestContent:     []byte("a"),
		MaxConcurrency:      6,
		Iterations:          5,
		OnWorkloadFailure:   "retry",
		MaxWorkloadFailures: 0,
	}
	variants := []Params{
		base,
		{Provider: "GCP", Product: base.Product, Plan: "Enterprise", Workload: base.Workload, ManifestContent: base.ManifestContent, MaxConcurrency: base.MaxConcurrency, Iterations: base.Iterations, OnWorkloadFailure: base.OnWorkloadFailure},
		{Provider: base.Provider, Product: base.Product, Plan: base.Plan, Workload: "tpce", ManifestContent: base.ManifestContent, MaxConcurrency: base.MaxConcurrency, Iterations: base.Iterations, OnWorkloadFailure: base.OnWorkloadFailure},
		{Provider: base.Provider, Product: "Aurora", Workload: base.Workload, ManifestContent: base.ManifestContent, MaxConcurrency: base.MaxConcurrency, Iterations: base.Iterations, OnWorkloadFailure: base.OnWorkloadFailure},
		{Provider: base.Provider, Product: base.Product, Plan: base.Plan, Workload: base.Workload, ManifestContent: []byte("b"), MaxConcurrency: base.MaxConcurrency, Iterations: base.Iterations, OnWorkloadFailure: base.OnWorkloadFailure},
		{Provider: base.Provider, Product: base.Product, Plan: base.Plan, Workload: base.Workload, ManifestContent: base.ManifestContent, MaxConcurrency: 3, Iterations: base.Iterations, OnWorkloadFailure: base.OnWorkloadFailure},
		{Provider: base.Provider, Product: base.Product, Plan: base.Plan, Workload: base.Workload, ManifestContent: base.ManifestContent, MaxConcurrency: base.MaxConcurrency, Iterations: 10, OnWorkloadFailure: base.OnWorkloadFailure},
		{Provider: base.Provider, Product: base.Product, Plan: base.Plan, Workload: base.Workload, ManifestContent: base.ManifestContent, MaxConcurrency: base.MaxConcurrency, Iterations: base.Iterations, OnWorkloadFailure: "fail-keep"},
		{Provider: base.Provider, Product: base.Product, Plan: base.Plan, Workload: base.Workload, ManifestContent: base.ManifestContent, MaxConcurrency: base.MaxConcurrency, Iterations: base.Iterations, OnWorkloadFailure: base.OnWorkloadFailure, MaxWorkloadFailures: 2},
		{Provider: base.Provider, Product: base.Product, Plan: base.Plan, Workload: base.Workload, ManifestContent: base.ManifestContent, MaxConcurrency: base.MaxConcurrency, Iterations: base.Iterations, OnWorkloadFailure: base.OnWorkloadFailure, ManifestParams: map[string]string{"supabase_org_id": "org-a"}},
		{Provider: base.Provider, Product: base.Product, Plan: base.Plan, Workload: base.Workload, ManifestContent: base.ManifestContent, MaxConcurrency: base.MaxConcurrency, Iterations: base.Iterations, OnWorkloadFailure: base.OnWorkloadFailure, ManifestParams: map[string]string{"supabase_org_id": "org-b"}},
	}

	seen := map[string]bool{}
	for i, v := range variants {
		id := Compute(v)
		if seen[id] {
			t.Errorf("variant %d produced a duplicate id %q", i, id)
		}
		seen[id] = true
	}
}

func TestCompute_ManifestParamsOrderIndependent(t *testing.T) {
	p1 := Params{Provider: "Supabase", Product: "Supabase", Plan: "Pro", ManifestParams: map[string]string{"a": "1", "b": "2"}}
	p2 := Params{Provider: "Supabase", Product: "Supabase", Plan: "Pro", ManifestParams: map[string]string{"b": "2", "a": "1"}}
	if Compute(p1) != Compute(p2) {
		t.Error("map iteration order should not affect the computed id")
	}
}

// TestCompute_UnscopedHashUnaffectedByTestPointScopeField pins that an
// unscoped Params (TestPointScope == "") hashes exactly the byte stream it
// did before TestPointScope existed, by independently reimplementing that
// exact pre-existing stream as an oracle. If this ever starts failing, every
// already-existing sweep id (including any in-progress real sweep) would
// silently stop being recognized by `run`/`resume`.
func TestCompute_UnscopedHashUnaffectedByTestPointScopeField(t *testing.T) {
	p := Params{
		Provider: "Supabase", Product: "Supabase", Plan: "Pro", Workload: "tpcc",
		ManifestContent: []byte("provider: Supabase\n"), MaxConcurrency: 8, Iterations: 3,
		OnWorkloadFailure: "retry", MaxWorkloadFailures: 0,
	}
	got := Compute(p)

	h := sha256.New()
	fmt.Fprintf(h, "provider=%s\n", p.Provider)
	fmt.Fprintf(h, "product=%s\n", p.Product)
	fmt.Fprintf(h, "plan=%s\n", p.Plan)
	fmt.Fprintf(h, "workload=%s\n", p.Workload)
	fmt.Fprintf(h, "max_concurrency=%d\n", p.MaxConcurrency)
	fmt.Fprintf(h, "iterations=%d\n", p.Iterations)
	fmt.Fprintf(h, "on_workload_failure=%s\n", p.OnWorkloadFailure)
	fmt.Fprintf(h, "max_workload_failures=%d\n", p.MaxWorkloadFailures)
	h.Write(p.ManifestContent)
	want := "supabase-pro-tpcc-" + hex.EncodeToString(h.Sum(nil))[:12]

	if got != want {
		t.Errorf("Compute() = %q, want %q (an unscoped id must never be affected by the TestPointScope field)", got, want)
	}
}

func TestCompute_TestPointScope_DistinctFromUnscoped(t *testing.T) {
	base := Params{Provider: "Supabase", Product: "Supabase", Plan: "Pro", Workload: "tpcc", ManifestContent: []byte("x")}
	unscoped := Compute(base)

	scoped := base
	scoped.TestPointScope = "small/io"
	got := Compute(scoped)

	if got == unscoped {
		t.Fatal("a scoped and unscoped sweep over otherwise-identical params must not collide on the same id")
	}
	if !strings.HasPrefix(got, "supabase-pro-tpcc-small-io-") {
		t.Errorf("scoped id = %q, want the test point folded into the visible prefix", got)
	}
}

func TestCompute_DifferentTestPointScopesDifferentIds(t *testing.T) {
	base := Params{Provider: "Supabase", Product: "Supabase", Plan: "Pro", Workload: "tpcc", ManifestContent: []byte("x")}
	a, b := base, base
	a.TestPointScope = "small/io"
	b.TestPointScope = "large/io/matched-to-rds"
	if Compute(a) == Compute(b) {
		t.Fatal("different test point scopes must not collide")
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"aws/rds":         "aws-rds",
		"GCP/CloudSQL":    "gcp-cloudsql",
		"local":           "local",
		"a--b":            "a-b",
		"/leading/edge/":  "leading-edge",
		"Enterprise Plus": "enterprise-plus",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

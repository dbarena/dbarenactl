package sweepid

import (
	"strings"
	"testing"
)

func TestCompute_Deterministic(t *testing.T) {
	p := Params{
		Provider:        "aws/rds",
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

func TestCompute_ProviderWorkloadSlugPrefix(t *testing.T) {
	p := Params{Provider: "aws/rds", Workload: "tpcc"}
	id := Compute(p)
	if got, want := id[:len("aws-rds-tpcc-")], "aws-rds-tpcc-"; got != want {
		t.Errorf("id = %q, want prefix %q", id, want)
	}
}

func TestCompute_DifferentWorkloadDifferentPrefix(t *testing.T) {
	tpcc := Compute(Params{Provider: "aws/rds", Workload: "tpcc"})
	tpce := Compute(Params{Provider: "aws/rds", Workload: "tpce"})
	if tpcc == tpce {
		t.Fatal("different workloads should never collide")
	}
	if strings.HasPrefix(tpcc, "aws-rds-tpce-") || strings.HasPrefix(tpce, "aws-rds-tpcc-") {
		t.Errorf("ids should be visually distinguishable by workload, got %q and %q", tpcc, tpce)
	}
}

func TestCompute_DiffersOnAnyParamChange(t *testing.T) {
	base := Params{
		Provider:            "aws/rds",
		Workload:            "tpcc",
		ManifestContent:     []byte("a"),
		MaxConcurrency:      6,
		Iterations:          5,
		OnWorkloadFailure:   "retry",
		MaxWorkloadFailures: 0,
	}
	variants := []Params{
		base,
		{Provider: "gcp/cloudsql", Workload: base.Workload, ManifestContent: base.ManifestContent, MaxConcurrency: base.MaxConcurrency, Iterations: base.Iterations, OnWorkloadFailure: base.OnWorkloadFailure},
		{Provider: base.Provider, Workload: "tpce", ManifestContent: base.ManifestContent, MaxConcurrency: base.MaxConcurrency, Iterations: base.Iterations, OnWorkloadFailure: base.OnWorkloadFailure},
		{Provider: base.Provider, ManifestContent: []byte("b"), MaxConcurrency: base.MaxConcurrency, Iterations: base.Iterations, OnWorkloadFailure: base.OnWorkloadFailure},
		{Provider: base.Provider, ManifestContent: base.ManifestContent, MaxConcurrency: 3, Iterations: base.Iterations, OnWorkloadFailure: base.OnWorkloadFailure},
		{Provider: base.Provider, ManifestContent: base.ManifestContent, MaxConcurrency: base.MaxConcurrency, Iterations: 10, OnWorkloadFailure: base.OnWorkloadFailure},
		{Provider: base.Provider, ManifestContent: base.ManifestContent, MaxConcurrency: base.MaxConcurrency, Iterations: base.Iterations, OnWorkloadFailure: "fail-keep"},
		{Provider: base.Provider, ManifestContent: base.ManifestContent, MaxConcurrency: base.MaxConcurrency, Iterations: base.Iterations, OnWorkloadFailure: base.OnWorkloadFailure, MaxWorkloadFailures: 2},
		{Provider: base.Provider, ManifestContent: base.ManifestContent, MaxConcurrency: base.MaxConcurrency, Iterations: base.Iterations, OnWorkloadFailure: base.OnWorkloadFailure, ManifestParams: map[string]string{"supabase_org_id": "org-a"}},
		{Provider: base.Provider, ManifestContent: base.ManifestContent, MaxConcurrency: base.MaxConcurrency, Iterations: base.Iterations, OnWorkloadFailure: base.OnWorkloadFailure, ManifestParams: map[string]string{"supabase_org_id": "org-b"}},
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
	p1 := Params{Provider: "supabase", ManifestParams: map[string]string{"a": "1", "b": "2"}}
	p2 := Params{Provider: "supabase", ManifestParams: map[string]string{"b": "2", "a": "1"}}
	if Compute(p1) != Compute(p2) {
		t.Error("map iteration order should not affect the computed id")
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"aws/rds":        "aws-rds",
		"GCP/CloudSQL":   "gcp-cloudsql",
		"local":          "local",
		"a--b":           "a-b",
		"/leading/edge/": "leading-edge",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

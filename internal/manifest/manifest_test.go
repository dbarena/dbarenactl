package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validYAML = `
provider: AWS
product: RDS
workload: tpcc
scenario_path: rds-tpcc-ec2-tiers.yaml
test_points:
  - tier: small
    bound_type: io
    set:
      project_size: small
      disk_iops: "3000"
  - tier: small
    bound_type: compute
    set:
      project_size: small
      disk_iops: "0"
`

func TestParse_Valid(t *testing.T) {
	m, err := parse([]byte(validYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Provider != "AWS" {
		t.Errorf("Provider = %q", m.Provider)
	}
	if m.PricingFetcherKey() != "aws/rds" {
		t.Errorf("PricingFetcherKey() = %q, want %q", m.PricingFetcherKey(), "aws/rds")
	}
	if len(m.TestPoints) != 2 {
		t.Fatalf("TestPoints = %d, want 2", len(m.TestPoints))
	}
	if m.TestPoints[0].Set["project_size"] != "small" {
		t.Errorf("test point 0 set[project_size] = %q", m.TestPoints[0].Set["project_size"])
	}
}

func TestParse_MissingRequiredFields(t *testing.T) {
	cases := map[string]string{
		"provider":      "product: RDS\nworkload: tpcc\nscenario_path: x.yaml\ntest_points:\n  - {tier: small, bound_type: io}\n",
		"workload":      "provider: AWS\nproduct: RDS\nscenario_path: x.yaml\ntest_points:\n  - {tier: small, bound_type: io}\n",
		"scenario_path": "provider: AWS\nproduct: RDS\nworkload: tpcc\ntest_points:\n  - {tier: small, bound_type: io}\n",
		"test_points":   "provider: AWS\nproduct: RDS\nworkload: tpcc\nscenario_path: x.yaml\n",
	}
	for name, yamlSrc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parse([]byte(yamlSrc))
			if err == nil {
				t.Fatalf("expected error for missing %s", name)
			}
		})
	}
}

func TestParse_MissingTierOrBoundType(t *testing.T) {
	_, err := parse([]byte("provider: AWS\nproduct: RDS\nworkload: tpcc\nscenario_path: x.yaml\ntest_points:\n  - {tier: small}\n"))
	if err == nil || !strings.Contains(err.Error(), "bound_type") {
		t.Fatalf("expected bound_type error, got: %v", err)
	}
}

func TestParse_DuplicateTestPoint(t *testing.T) {
	src := "provider: AWS\nproduct: RDS\nworkload: tpcc\nscenario_path: x.yaml\ntest_points:\n" +
		"  - {tier: small, bound_type: io}\n" +
		"  - {tier: small, bound_type: io}\n"
	_, err := parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate error, got: %v", err)
	}
}

func TestTestPointDef_Key(t *testing.T) {
	d := TestPointDef{Tier: "small", BoundType: "io"}
	if d.Key() != "small/io" {
		t.Errorf("Key() = %q", d.Key())
	}
	d.Variant = "matched-to-rds"
	if d.Key() != "small/io/matched-to-rds" {
		t.Errorf("Key() with variant = %q", d.Key())
	}
}

func TestLoad_ResolvesRelativeScenarioPath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "aws-rds.yaml")
	if err := os.WriteFile(p, []byte(validYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := filepath.Join(dir, "rds-tpcc-ec2-tiers.yaml")
	if m.ResolvedScenarioPath() != want {
		t.Errorf("ResolvedScenarioPath() = %q, want %q", m.ResolvedScenarioPath(), want)
	}
}

func TestLoad_ResolvesRelativeCandidatePathToAbsoluteScenarioPath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "aws-rds.yaml")
	if err := os.WriteFile(p, []byte(validYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relPath, err := filepath.Rel(cwd, p)
	if err != nil {
		t.Fatal(err)
	}

	m, err := Load(relPath)
	if err != nil {
		t.Fatalf("Load(%q): %v", relPath, err)
	}
	if !filepath.IsAbs(m.ResolvedScenarioPath()) {
		t.Fatalf("ResolvedScenarioPath() = %q, want an absolute path", m.ResolvedScenarioPath())
	}
	want := filepath.Join(dir, "rds-tpcc-ec2-tiers.yaml")
	if m.ResolvedScenarioPath() != want {
		t.Errorf("ResolvedScenarioPath() = %q, want %q", m.ResolvedScenarioPath(), want)
	}
}

func TestLoad_AbsoluteScenarioPathUnchanged(t *testing.T) {
	dir := t.TempDir()
	src := "provider: AWS\nproduct: RDS\nworkload: tpcc\nscenario_path: /opt/benchctl/scenarios/x.yaml\ntest_points:\n  - {tier: small, bound_type: io}\n"
	p := filepath.Join(dir, "aws-rds.yaml")
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.ResolvedScenarioPath() != "/opt/benchctl/scenarios/x.yaml" {
		t.Errorf("ResolvedScenarioPath() = %q", m.ResolvedScenarioPath())
	}
}

func TestLoad_NonexistentFile(t *testing.T) {
	if _, err := Load("/nonexistent/path.yaml"); err == nil {
		t.Fatal("expected error for nonexistent manifest")
	}
}

func manifestWithParam(t *testing.T) *Manifest {
	t.Helper()
	src := "provider: Supabase\nproduct: Supabase\nplan: Pro\nworkload: tpcc\nscenario_path: x.yaml\ntest_points:\n" +
		"  - {tier: small, bound_type: io, set: {supabase_org_id: \"{{ params.supabase_org_id }}\", warehouses: \"80\"}}\n" +
		"  - {tier: small, bound_type: compute, set: {supabase_org_id: \"{{ params.supabase_org_id }}\", warehouses: \"14\"}}\n"
	m, err := parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return m
}

func TestResolveParams_SubstitutesAcrossAllTestPoints(t *testing.T) {
	m := manifestWithParam(t)
	if err := m.ResolveParams(map[string]string{"supabase_org_id": "abc1234"}); err != nil {
		t.Fatalf("ResolveParams: %v", err)
	}
	for _, tp := range m.TestPoints {
		if tp.Set["supabase_org_id"] != "abc1234" {
			t.Errorf("test point %s: supabase_org_id = %q", tp.Key(), tp.Set["supabase_org_id"])
		}
	}
	if m.TestPoints[0].Set["warehouses"] != "80" {
		t.Error("unrelated values must be left untouched")
	}
}

func TestResolveParams_MissingSingleParam(t *testing.T) {
	m := manifestWithParam(t)
	err := m.ResolveParams(nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	want := "the parameter supabase_org_id is required. Set it via --set supabase_org_id=<value>"
	if err.Error() != want {
		t.Errorf("err = %q, want %q", err.Error(), want)
	}
}

func TestResolveParams_MissingMultipleParamsReportedTogether(t *testing.T) {
	src := "provider: AWS\nproduct: RDS\nworkload: tpcc\nscenario_path: x.yaml\ntest_points:\n" +
		"  - {tier: small, bound_type: io, set: {a: \"{{ params.foo }}\", b: \"{{ params.bar }}\"}}\n"
	m, err := parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	err = m.ResolveParams(nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "--set bar=<value>") || !strings.Contains(err.Error(), "--set foo=<value>") {
		t.Errorf("err = %q, should hint at both missing params", err)
	}
}

func TestResolveParams_NoPlaceholdersIsNoop(t *testing.T) {
	m, err := parse([]byte(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ResolveParams(nil); err != nil {
		t.Fatalf("ResolveParams on a manifest with no placeholders: %v", err)
	}
}

func TestResolveParams_IdempotentOnAlreadyResolvedInput(t *testing.T) {
	m := manifestWithParam(t)
	params := map[string]string{"supabase_org_id": "abc1234"}
	if err := m.ResolveParams(params); err != nil {
		t.Fatal(err)
	}
	if err := m.ResolveParams(params); err != nil {
		t.Fatalf("second ResolveParams call: %v", err)
	}
	if m.TestPoints[0].Set["supabase_org_id"] != "abc1234" {
		t.Errorf("value changed on second resolve: %q", m.TestPoints[0].Set["supabase_org_id"])
	}
}

// --- Provider/Product/Plan derivation and validation ---

func baseYAML(fields string) string {
	return fields + "\nworkload: tpcc\nscenario_path: x.yaml\ntest_points:\n  - {tier: small, bound_type: io}\n"
}

func TestPricingFetcherKey_IgnoresPlan(t *testing.T) {
	cases := []struct {
		name           string
		fields         string
		wantFetcherKey string
	}{
		{"aws/rds", "provider: AWS\nproduct: RDS", "aws/rds"},
		{"gcp/cloudsql (Enterprise)", "provider: GCP\nproduct: Cloud SQL for Postgres\nplan: Enterprise", "gcp/cloudsql"},
		{"gcp/cloudsql (Enterprise Plus)", "provider: GCP\nproduct: Cloud SQL for Postgres\nplan: Enterprise Plus", "gcp/cloudsql"},
		{"supabase", "provider: Supabase\nproduct: Supabase\nplan: Pro", "supabase"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := parse([]byte(baseYAML(tc.fields)))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := m.PricingFetcherKey(); got != tc.wantFetcherKey {
				t.Errorf("PricingFetcherKey() = %q, want %q", got, tc.wantFetcherKey)
			}
		})
	}
}

func TestValidate_RejectsUnknownProvider(t *testing.T) {
	_, err := parse([]byte(baseYAML("provider: Azure\nproduct: SQL")))
	if err == nil || !strings.Contains(err.Error(), "provider must be one of") {
		t.Fatalf("expected an unknown-provider error, got: %v", err)
	}
}

func TestValidate_RejectsMismatchedProduct(t *testing.T) {
	_, err := parse([]byte(baseYAML("provider: AWS\nproduct: Aurora")))
	if err == nil || !strings.Contains(err.Error(), `requires product "RDS"`) {
		t.Fatalf("expected a product-mismatch error, got: %v", err)
	}
}

func TestValidate_GCPRequiresPlan(t *testing.T) {
	_, err := parse([]byte(baseYAML("provider: GCP\nproduct: Cloud SQL for Postgres")))
	if err == nil || !strings.Contains(err.Error(), "requires plan to be one of") {
		t.Fatalf("expected a missing-plan error, got: %v", err)
	}
}

func TestValidate_GCPRejectsUnknownPlan(t *testing.T) {
	_, err := parse([]byte(baseYAML("provider: GCP\nproduct: Cloud SQL for Postgres\nplan: Standard")))
	if err == nil || !strings.Contains(err.Error(), "requires plan to be one of") {
		t.Fatalf("expected an unknown-plan error, got: %v", err)
	}
}

func TestValidate_SupabaseRequiresPlanPro(t *testing.T) {
	_, err := parse([]byte(baseYAML("provider: Supabase\nproduct: Supabase")))
	if err == nil || !strings.Contains(err.Error(), "requires plan to be one of") {
		t.Fatalf("expected a missing-plan error, got: %v", err)
	}
}

func TestValidate_AWSRejectsPlan(t *testing.T) {
	_, err := parse([]byte(baseYAML("provider: AWS\nproduct: RDS\nplan: Enterprise")))
	if err == nil || !strings.Contains(err.Error(), "does not use a plan") {
		t.Fatalf("expected a plan-not-applicable error, got: %v", err)
	}
}

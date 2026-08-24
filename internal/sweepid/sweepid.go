// Package sweepid computes the deterministic identifier for a dbarenactl sweep.
//
// A sweep is identified by provider + its full parameter set (the manifest
// content and every flag that affects what work gets done), not by provider
// alone: two invocations that differ in any of these are different sweeps
// and can run concurrently; an identical repeated invocation resolves to the
// same id and resumes the existing sweep instead of starting a duplicate.
package sweepid

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Params is everything that determines a sweep's identity. Any change to any
// field here is, by design, a different sweep -- not a continuation of an
// existing one.
type Params struct {
	Provider string
	// Workload is the manifest's own declared workload (e.g. "tpcc"),
	// folded into both the id's readable prefix and its hash -- two sweeps
	// for the same provider but different workloads must be visually
	// distinguishable, not just hash-different.
	Workload            string
	ManifestContent     []byte
	MaxConcurrency      int
	Iterations          int
	OnWorkloadFailure   string
	MaxWorkloadFailures int
	// ManifestParams holds the resolved `--set NAME=value` values used to
	// fill in a manifest's `{{ params.NAME }}` placeholders (see
	// internal/manifest's ResolveParams). A different value here is a
	// genuinely different sweep -- e.g. a different supabase_org_id points
	// at different target infrastructure entirely -- not a continuation of
	// an existing one.
	ManifestParams map[string]string
}

var nonSlugChars = regexp.MustCompile(`[^a-z0-9]+`)

// Compute returns a readable provider+workload slug followed by a short
// hash of every parameter in p, e.g. "aws-rds-tpcc-3f9a1c2b8e47".
func Compute(p Params) string {
	h := sha256.New()
	fmt.Fprintf(h, "provider=%s\n", p.Provider)
	fmt.Fprintf(h, "workload=%s\n", p.Workload)
	fmt.Fprintf(h, "max_concurrency=%d\n", p.MaxConcurrency)
	fmt.Fprintf(h, "iterations=%d\n", p.Iterations)
	fmt.Fprintf(h, "on_workload_failure=%s\n", p.OnWorkloadFailure)
	fmt.Fprintf(h, "max_workload_failures=%d\n", p.MaxWorkloadFailures)
	keys := make([]string, 0, len(p.ManifestParams))
	for k := range p.ManifestParams {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "param.%s=%s\n", k, p.ManifestParams[k])
	}
	h.Write(p.ManifestContent)
	sum := hex.EncodeToString(h.Sum(nil))[:12]
	return slugify(p.Provider) + "-" + slugify(p.Workload) + "-" + sum
}

// slugify lowercases s and collapses any run of non-alphanumeric characters
// (e.g. "/" in "aws/rds") into a single dash, trimming leading/trailing dashes.
func slugify(s string) string {
	s = nonSlugChars.ReplaceAllString(strings.ToLower(s), "-")
	return strings.Trim(s, "-")
}

// Slugify is the exported form of slugify, reused by other packages that
// need to turn arbitrary identifying strings (tiers, bound types, variants)
// into characters safe for both SQLite row ids and benchctl run ids (which
// reject "/" and other punctuation -- see runstate.ValidateRunID upstream).
func Slugify(s string) string { return slugify(s) }

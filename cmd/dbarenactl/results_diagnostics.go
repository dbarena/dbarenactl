package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// supabaseAddonsFile is benchctl's diagnostics snapshot of
// GET /v1/projects/{ref}/billing/addons, written to every Supabase run's
// artifact dir as diagnostics/addons.json (see
// benchctl/internal/diagnostics/supabase/config.go). selected_addons is the
// only field benchctl keeps from that response.
type supabaseAddonsFile struct {
	SelectedAddons []struct {
		Type    string `json:"type"`
		Variant struct {
			Meta struct {
				CPUCores float64 `json:"cpu_cores"`
				MemoryGB float64 `json:"memory_gb"`
			} `json:"meta"`
		} `json:"variant"`
	} `json:"selected_addons"`
}

// supabaseComputeSize reads vcpu/ram_gb for a Supabase run from its own
// diagnostics/addons.json -- the actual compute addon the project ran with,
// as reported live by the Management API at run time. This is the sole
// source for Supabase vcpu/ram_gb (see cost_registry.go's newVCPURAMFuncs
// doc comment for why pricing.md can't fill this role): it returns nil, nil
// on any failure (missing file, malformed JSON, no compute_instance entry)
// so a run with no diagnostics still produces a result, just with
// vcpu/ram_gb left null.
func supabaseComputeSize(artifactDir string) (vcpu, ramGB *float64) {
	data, err := os.ReadFile(filepath.Join(artifactDir, "diagnostics", "addons.json"))
	if err != nil {
		return nil, nil
	}
	var parsed supabaseAddonsFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, nil
	}
	for _, addon := range parsed.SelectedAddons {
		if addon.Type != "compute_instance" {
			continue
		}
		v, r := addon.Variant.Meta.CPUCores, addon.Variant.Meta.MemoryGB
		return &v, &r
	}
	return nil, nil
}

// supabaseDiskFile is benchctl's diagnostics snapshot of
// GET /v1/projects/{ref}/config/disk, written to every Supabase run's
// artifact dir as diagnostics/disk.json (see
// benchctl/internal/diagnostics/supabase/config.go).
type supabaseDiskFile struct {
	Attributes struct {
		Type string `json:"type"`
	} `json:"attributes"`
}

// supabaseDiskType reads disk_type for a Supabase run from its own
// diagnostics/disk.json -- the actual disk type the project ran with, as
// reported live by the Management API at run time. Returns nil on any
// failure (missing file, malformed JSON, empty type) so a run with no
// diagnostics falls back to whatever the manifest declared (see
// buildResultDoc).
func supabaseDiskType(artifactDir string) *string {
	data, err := os.ReadFile(filepath.Join(artifactDir, "diagnostics", "disk.json"))
	if err != nil {
		return nil
	}
	var parsed supabaseDiskFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil
	}
	if parsed.Attributes.Type == "" {
		return nil
	}
	return &parsed.Attributes.Type
}

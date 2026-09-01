package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dbarena/dbarenactl/internal/pricing"
)

// appendPricingAuditLog records exactly how one scenario's monthly_usd was
// computed to ~/.dbarenactl/sweeps/<sweep-id>/logs/pricing-audit.log --
// never printed to the CLI, so normal `results` output stays readable, but
// always available so a cost figure can be checked component-by-component
// before a result.json is submitted in a PR. Appends one block per call;
// never overwrites prior runs, so a sweep's full pricing history stays
// auditable even if `results` is re-run after a pricing snapshot changes.
func appendPricingAuditLog(sweepID, scenarioPath string, in pricing.CostInput, snap *pricing.Snapshot, bd *pricing.CostBreakdown) error {
	logDir, err := logsBaseDir(sweepID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("pricing audit log: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(logDir, "pricing-audit.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("pricing audit log: %w", err)
	}
	defer f.Close() //nolint:errcheck

	fmt.Fprintf(f, "=== %s pricing audit: %s ===\n", time.Now().UTC().Format(time.RFC3339), scenarioPath)
	fmt.Fprintf(f, "instance_type=%s disk_type=%s disk_gb=%g iops=%g throughput_mbps=%g\n",
		in.InstanceType, in.DiskType, in.DiskGB, in.IOPS, in.ThroughputMbps)
	fmt.Fprintf(f, "pricing snapshot: id=%s fetched_at=%s source=%s\n",
		snap.ID, snap.FetchedAt.Format(time.RFC3339), snap.Source)
	for _, c := range bd.Components {
		fmt.Fprintf(f, "%-20s %s\n", c.Name+":", c.Detail)
	}
	fmt.Fprintf(f, "TOTAL monthly_usd = $%.2f\n\n", bd.TotalUSD)
	return nil
}

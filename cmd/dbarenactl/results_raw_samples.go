package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// rawSamplesPathFor derives the raw_samples_*.csv path benchctl's stdout
// collector wrote alongside resultsJSONPath. Both are written by the same
// Collect() call with the same naming inputs (benchctl's
// internal/collectors/stdout/collector.go, artifactNaming/artifactFilename),
// so their <benchmark>_<iteration>[_<fixture>] suffix is always identical --
// deriving the path this way avoids re-parsing the raw-samples filename
// independently, which would be ambiguous once a benchmark name itself
// contains underscores.
func rawSamplesPathFor(resultsJSONPath string) string {
	dir, base := filepath.Split(resultsJSONPath)
	base = strings.TrimSuffix(base, ".json")
	base = strings.Replace(base, "results_", "raw_samples_", 1)
	return filepath.Join(dir, base+".csv")
}

// rawSamplesColumns are the columns writeRawClientsCSV reads from a benchctl
// raw_samples_*.csv (go-tpc's --raw-samples-file) -- one row per (tick,
// transaction, status).
var rawSamplesColumns = []string{
	"t_seconds", "transaction", "status", "tpm",
	"avg_latency_ms", "p50_latency_ms", "p90_latency_ms", "p95_latency_ms",
	"p99_latency_ms", "p99_9_latency_ms", "max_latency_ms",
}

// rawClientsHeader is dbarena results/'s raw-clients-<threads>.csv header.
// Scoped to the primary transaction type only.
var rawClientsHeader = []string{
	"t_seconds", "new_order_tpm", "new_order_avg_latency_ms",
	"new_order_p50_latency_ms", "new_order_p90_latency_ms",
	"new_order_p95_latency_ms", "new_order_p99_latency_ms",
	"new_order_p99_9_latency_ms", "new_order_max_latency_ms",
}

// newOrderRow is one NEW_ORDER/ok tick, kept in rawClientsHeader's column
// order. Field values are the original strings go-tpc produced -- never
// round-tripped through float64 -- except tSeconds, parsed transiently only
// to sort rows numerically rather than lexically.
type newOrderRow struct {
	tSeconds float64
	fields   []string
}

// writeRawClientsCSV reads a benchctl raw_samples_*.csv, keeps only
// NEW_ORDER/ok rows -- the "only copy success rows" policy -- and writes
// them to destPath in dbarena's raw-clients-<threads>.csv shape.
func writeRawClientsCSV(srcPath, destPath string) error {
	rows, err := readNewOrderOkRows(srcPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", srcPath, err)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].tSeconds < rows[j].tSeconds })

	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", destPath, err)
	}
	defer out.Close() //nolint:errcheck

	w := csv.NewWriter(out)
	if err := w.Write(rawClientsHeader); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	for _, r := range rows {
		if err := w.Write(r.fields); err != nil {
			return fmt.Errorf("write row: %w", err)
		}
	}
	w.Flush()
	return w.Error()
}

// readNewOrderOkRows reads srcPath and returns every NEW_ORDER/ok row,
// projected to rawClientsHeader's columns. Column positions are looked up
// by header name, not fixed index, so a reordering upstream doesn't
// silently scramble values.
func readNewOrderOkRows(srcPath string) ([]newOrderRow, error) {
	f, err := os.Open(srcPath)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck

	r := csv.NewReader(f)
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	idx := make(map[string]int, len(header))
	for i, name := range header {
		idx[name] = i
	}
	for _, want := range rawSamplesColumns {
		if _, ok := idx[want]; !ok {
			return nil, fmt.Errorf("missing expected column %q", want)
		}
	}

	var rows []newOrderRow
	for {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read row: %w", err)
		}
		if record[idx["transaction"]] != "NEW_ORDER" || record[idx["status"]] != "ok" {
			continue
		}
		tSecondsStr := record[idx["t_seconds"]]
		tSeconds, err := strconv.ParseFloat(tSecondsStr, 64)
		if err != nil {
			return nil, fmt.Errorf("t_seconds %q: %w", tSecondsStr, err)
		}
		rows = append(rows, newOrderRow{
			tSeconds: tSeconds,
			fields: []string{
				tSecondsStr,
				record[idx["tpm"]],
				record[idx["avg_latency_ms"]],
				record[idx["p50_latency_ms"]],
				record[idx["p90_latency_ms"]],
				record[idx["p95_latency_ms"]],
				record[idx["p99_latency_ms"]],
				record[idx["p99_9_latency_ms"]],
				record[idx["max_latency_ms"]],
			},
		})
	}
	return rows, nil
}

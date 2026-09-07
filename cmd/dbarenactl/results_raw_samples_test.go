package main

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"testing"
)

func TestRawSamplesPathFor(t *testing.T) {
	cases := []struct {
		resultsPath string
		want        string
	}{
		{"/runs/r1/results_supabase_1_12.json", "/runs/r1/raw_samples_supabase_1_12.csv"},
		{"/runs/r1/results_gcp_cloudsql_1_24.json", "/runs/r1/raw_samples_gcp_cloudsql_1_24.csv"},
		{"results_rds_1_4.json", "raw_samples_rds_1_4.csv"},
	}
	for _, c := range cases {
		got := rawSamplesPathFor(c.resultsPath)
		if got != c.want {
			t.Errorf("rawSamplesPathFor(%q) = %q, want %q", c.resultsPath, got, c.want)
		}
	}
}

const sampleRawSamplesCSV = `t_seconds,transaction,status,count,tpm,avg_latency_ms,p50_latency_ms,p90_latency_ms,p95_latency_ms,p99_latency_ms,p99_9_latency_ms,max_latency_ms
2.0,NEW_ORDER,ok,300,18000.2,3.0,2.8,4.1,4.7,6.2,10.5,22.0
2.0,PAYMENT,ok,290,17400.0,2.1,2.0,3.1,3.7,4.7,9.0,15.0
2.0,DELIVERY,ok,30,1800.0,5.5,5.2,7.9,8.4,9.4,14.2,23.1
1.0,NEW_ORDER,ok,250,15000.5,3.6,3.1,5.8,6.8,13.1,31.5,58.7
1.0,NEW_ORDER,error,1,3.0,6.0,6.3,6.3,6.3,6.3,6.3,6.3
1.0,DELIVERY,ok,35,2164.3,4.9,4.7,6.3,7.3,8.9,8.9,8.9
3.0,PAYMENT,ok,295,17700.0,2.2,2.0,3.2,3.8,4.8,9.1,15.1
`

func writeFixture(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", name, err)
	}
	return path
}

func readCSVRows(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("read csv %s: %v", path, err)
	}
	return rows
}

func TestWriteRawClientsCSV_FiltersToNewOrderOkAndSortsByTSeconds(t *testing.T) {
	dir := t.TempDir()
	src := writeFixture(t, dir, "raw_samples_rds_1_12.csv", sampleRawSamplesCSV)
	dest := filepath.Join(dir, "raw-clients-12.csv")

	if err := writeRawClientsCSV(src, dest); err != nil {
		t.Fatalf("writeRawClientsCSV: %v", err)
	}

	rows := readCSVRows(t, dest)
	wantHeader := []string{
		"t_seconds", "new_order_tpm", "new_order_avg_latency_ms",
		"new_order_p50_latency_ms", "new_order_p90_latency_ms",
		"new_order_p95_latency_ms", "new_order_p99_latency_ms",
		"new_order_p99_9_latency_ms", "new_order_max_latency_ms",
	}
	if len(rows) == 0 {
		t.Fatal("no rows written")
	}
	for i, want := range wantHeader {
		if rows[0][i] != want {
			t.Errorf("header[%d] = %q, want %q", i, rows[0][i], want)
		}
	}

	// Only the two NEW_ORDER/ok rows (t=1.0, t=2.0) survive -- the
	// NEW_ORDER/error row and the PAYMENT/DELIVERY rows must not appear.
	dataRows := rows[1:]
	if len(dataRows) != 2 {
		t.Fatalf("want 2 data rows, got %d: %v", len(dataRows), dataRows)
	}
	if dataRows[0][0] != "1.0" || dataRows[1][0] != "2.0" {
		t.Errorf("rows not sorted by t_seconds: %v", dataRows)
	}
	if dataRows[0][1] != "15000.5" {
		t.Errorf("row[0].new_order_tpm = %q, want 15000.5", dataRows[0][1])
	}
	if dataRows[0][2] != "3.6" {
		t.Errorf("row[0].new_order_avg_latency_ms = %q, want 3.6", dataRows[0][2])
	}
	if dataRows[1][1] != "18000.2" {
		t.Errorf("row[1].new_order_tpm = %q, want 18000.2", dataRows[1][1])
	}
}

func TestWriteRawClientsCSV_TickWithNoNewOrderOkRowIsOmitted(t *testing.T) {
	dir := t.TempDir()
	const content = `t_seconds,transaction,status,count,tpm,avg_latency_ms,p50_latency_ms,p90_latency_ms,p95_latency_ms,p99_latency_ms,p99_9_latency_ms,max_latency_ms
1.0,NEW_ORDER,ok,250,15000.5,3.6,3.1,5.8,6.8,13.1,31.5,58.7
2.0,NEW_ORDER,error,1,3.0,6.0,6.3,6.3,6.3,6.3,6.3,6.3
2.0,PAYMENT,ok,290,17400.0,2.1,2.0,3.1,3.7,4.7,9.0,15.0
`
	src := writeFixture(t, dir, "raw_samples_rds_1_12.csv", content)
	dest := filepath.Join(dir, "raw-clients-12.csv")

	if err := writeRawClientsCSV(src, dest); err != nil {
		t.Fatalf("writeRawClientsCSV: %v", err)
	}

	rows := readCSVRows(t, dest)
	if len(rows) != 2 { // header + the one t=1.0 row; t=2.0 has no NEW_ORDER/ok row
		t.Fatalf("want 1 data row (t=1.0 only), got %d rows total: %v", len(rows)-1, rows)
	}
	if rows[1][0] != "1.0" {
		t.Errorf("surviving row t_seconds = %q, want 1.0", rows[1][0])
	}
}

func TestWriteRawClientsCSV_MissingSourceErrors(t *testing.T) {
	dir := t.TempDir()
	err := writeRawClientsCSV(filepath.Join(dir, "does-not-exist.csv"), filepath.Join(dir, "out.csv"))
	if err == nil {
		t.Fatal("expected error for missing source file")
	}
}

func TestWriteRawClientsCSV_MissingColumnErrors(t *testing.T) {
	dir := t.TempDir()
	const content = "t_seconds,transaction,status,tpm\n1.0,NEW_ORDER,ok,600\n"
	src := writeFixture(t, dir, "raw_samples_rds_1_12.csv", content)
	err := writeRawClientsCSV(src, filepath.Join(dir, "out.csv"))
	if err == nil {
		t.Fatal("expected error for source missing latency columns")
	}
}

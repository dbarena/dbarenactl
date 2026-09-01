package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func newTestSpinner(out *bytes.Buffer, isTTY bool, message string) *Spinner {
	return &Spinner{message: message, out: out, isTTY: isTTY}
}

func TestSpinnerNonTTYSucceed(t *testing.T) {
	var buf bytes.Buffer
	sp := newTestSpinner(&buf, false, "Fetching GCP pricing for us-central1")
	sp.Start()
	sp.Succeed("Fetched GCP pricing for us-central1 (42 items, 8.4s)")

	got := buf.String()
	if !strings.Contains(got, "Fetching GCP pricing for us-central1...\n") {
		t.Errorf("missing start line, got %q", got)
	}
	if !strings.Contains(got, "✓ Fetched GCP pricing for us-central1 (42 items, 8.4s)\n") {
		t.Errorf("missing success line, got %q", got)
	}
	if strings.Contains(got, "\r") {
		t.Errorf("non-TTY output must not contain carriage returns, got %q", got)
	}
}

func TestSpinnerNonTTYFail(t *testing.T) {
	var buf bytes.Buffer
	sp := newTestSpinner(&buf, false, "Fetching AWS pricing for us-east-1")
	sp.Start()
	sp.Fail("Fetching AWS pricing for us-east-1 failed: unexpected status 500")

	got := buf.String()
	if !strings.Contains(got, "✗ Fetching AWS pricing for us-east-1 failed: unexpected status 500\n") {
		t.Errorf("missing failure line, got %q", got)
	}
}

func TestSpinnerFinishIsIdempotent(t *testing.T) {
	var buf bytes.Buffer
	sp := newTestSpinner(&buf, false, "op")
	sp.Start()
	sp.Succeed("done")
	sp.Fail("should be ignored")

	got := buf.String()
	if strings.Contains(got, "should be ignored") {
		t.Errorf("second finish call must be a no-op, got %q", got)
	}
}

func TestSpinnerTTYAnimateStopsCleanly(t *testing.T) {
	var buf bytes.Buffer
	sp := newTestSpinner(&buf, true, "animating")
	sp.Start()
	time.Sleep(3 * spinnerInterval)
	sp.Succeed("done animating")

	got := buf.String()
	if !strings.Contains(got, "✓ done animating\n") {
		t.Errorf("missing final success line, got %q", got)
	}
}

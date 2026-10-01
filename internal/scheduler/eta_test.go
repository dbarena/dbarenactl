package scheduler

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dbarena/dbarenactl/internal/sweepstate"
)

var etaNow = time.Date(2026, 10, 1, 13, 0, 0, 0, time.UTC)

func etaTestPoint(id string, needed, successes int) *sweepstate.TestPoint {
	return &sweepstate.TestPoint{ID: id, SuccessesNeeded: needed, SuccessesCount: successes, FailureBudget: 3}
}

// etaDoneRun is a successful, finalized run of tp that took d, provisioning
// included (provision 0 leaves ProvisionedAt unset, as for old runs).
func etaDoneRun(tp string, d, provision time.Duration) *sweepstate.Run {
	created := etaNow.Add(-24 * time.Hour)
	r := &sweepstate.Run{TestPointID: tp, Status: sweepstate.RunDone, Outcome: "success", CreatedAt: created, UpdatedAt: created.Add(d)}
	if provision > 0 {
		at := created.Add(provision)
		r.ProvisionedAt = &at
	}
	return r
}

func etaActiveRun(tp string, status sweepstate.RunStatus, elapsed time.Duration) *sweepstate.Run {
	return &sweepstate.Run{TestPointID: tp, Status: status, CreatedAt: etaNow.Add(-elapsed)}
}

// reportedSweep reproduces aws-tpcc-cache-fit-ab6fbf227139: 9 test points
// needing 3 successes with 1 each, all 12 slots busy, and 6 attempts not yet
// started. The old remaining-work/concurrency formula estimated ~5h even
// though every unstarted run alone takes 6h.
func reportedSweep() ([]*sweepstate.TestPoint, []*sweepstate.Run) {
	const avg = 6 * time.Hour
	ids := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
	var tps []*sweepstate.TestPoint
	var runs []*sweepstate.Run
	for _, id := range ids {
		tps = append(tps, etaTestPoint(id, 3, 1))
		runs = append(runs, etaDoneRun(id, avg, 0))
	}
	remaining := []time.Duration{
		0, 93 * time.Minute, 103 * time.Minute, 133 * time.Minute, 161 * time.Minute,
		159 * time.Minute, 172 * time.Minute, 181 * time.Minute, 196 * time.Minute,
	}
	for i, id := range ids {
		runs = append(runs, etaActiveRun(id, sweepstate.RunWaitingRemote, avg-remaining[i]))
	}
	// Attempt #3 of a, b and c is already running, so only d..i still need
	// a launch -- each as soon as one of the 12 slots frees up.
	for i, rem := range []time.Duration{206 * time.Minute, 224 * time.Minute, 234 * time.Minute} {
		runs = append(runs, etaActiveRun(ids[i], sweepstate.RunWaitingRemote, avg-rem))
	}
	return tps, runs
}

func TestEstimateRemaining(t *testing.T) {
	reportedTPs, reportedRuns := reportedSweep()

	tests := []struct {
		name        string
		testPoints  []*sweepstate.TestPoint
		runs        []*sweepstate.Run
		concurrency int
		want        time.Duration
		wantOK      bool
	}{
		{
			name:        "reported sweep: unstarted runs wait for a slot",
			testPoints:  reportedTPs,
			runs:        reportedRuns,
			concurrency: 12,
			// The 6th slot to free up does so after 2h41m.
			want:   161*time.Minute + 6*time.Hour,
			wantOK: true,
		},
		{
			name:       "spare slots, nothing left to launch: longest in-flight run",
			testPoints: []*sweepstate.TestPoint{etaTestPoint("a", 3, 1)},
			runs: []*sweepstate.Run{
				etaDoneRun("a", 3*time.Hour, 0),
				etaActiveRun("a", sweepstate.RunWaitingRemote, 2*time.Hour),
				etaActiveRun("a", sweepstate.RunWaitingRemote, 1*time.Hour),
			},
			concurrency: 4,
			want:        2 * time.Hour,
			wantOK:      true,
		},
		{
			name:        "concurrency 1 runs everything back to back",
			testPoints:  []*sweepstate.TestPoint{etaTestPoint("a", 2, 1), etaTestPoint("b", 2, 1)},
			runs:        []*sweepstate.Run{etaDoneRun("a", time.Hour, 0), etaDoneRun("b", 2*time.Hour, 0)},
			concurrency: 1,
			want:        3 * time.Hour,
			wantOK:      true,
		},
		{
			name:        "launches are serialized by provisioning time",
			testPoints:  []*sweepstate.TestPoint{etaTestPoint("a", 4, 1)},
			runs:        []*sweepstate.Run{etaDoneRun("a", time.Hour, 10*time.Minute)},
			concurrency: 3,
			// Launches at 0, 10m and 20m.
			want:   time.Hour + 20*time.Minute,
			wantOK: true,
		},
		{
			name:       "a provision in flight holds the launcher",
			testPoints: []*sweepstate.TestPoint{etaTestPoint("a", 3, 1)},
			runs: []*sweepstate.Run{
				etaDoneRun("a", time.Hour, 10*time.Minute),
				etaActiveRun("a", sweepstate.RunLaunching, 4*time.Minute),
			},
			concurrency: 2,
			// The next launch waits until the current provision finishes at 6m.
			want:   time.Hour + 6*time.Minute,
			wantOK: true,
		},
		{
			name:       "an overrunning run counts as finishing now",
			testPoints: []*sweepstate.TestPoint{etaTestPoint("a", 3, 1)},
			runs: []*sweepstate.Run{
				etaDoneRun("a", time.Hour, 0),
				etaActiveRun("a", sweepstate.RunWaitingRemote, 3*time.Hour),
			},
			concurrency: 1,
			want:        time.Hour,
			wantOK:      true,
		},
		{
			name: "skipped and satisfied test points are ignored",
			testPoints: []*sweepstate.TestPoint{
				etaTestPoint("a", 2, 1),
				etaTestPoint("done", 1, 1),
				{ID: "skipped", SuccessesNeeded: 3, FailureBudget: 3, Skipped: true},
			},
			runs:        []*sweepstate.Run{etaDoneRun("a", time.Hour, 0), etaDoneRun("done", 5*time.Hour, 0)},
			concurrency: 2,
			want:        time.Hour,
			wantOK:      true,
		},
		{
			name:        "a remaining test point without a success has no estimate",
			testPoints:  []*sweepstate.TestPoint{etaTestPoint("a", 2, 1), etaTestPoint("b", 2, 0)},
			runs:        []*sweepstate.Run{etaDoneRun("a", time.Hour, 0)},
			concurrency: 2,
			wantOK:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := successCounts(tt.testPoints)
			got, ok := EstimateRemaining(tt.testPoints, tt.runs, etaNow, tt.concurrency)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("EstimateRemaining = %v, want %v", got, tt.want)
			}
			if after := successCounts(tt.testPoints); after != before {
				t.Errorf("EstimateRemaining modified its input test points")
			}
		})
	}
}

func successCounts(tps []*sweepstate.TestPoint) string {
	var b strings.Builder
	for _, tp := range tps {
		fmt.Fprintf(&b, "%s=%d ", tp.ID, tp.SuccessesCount)
	}
	return b.String()
}

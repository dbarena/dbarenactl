package scheduler

import (
	"sort"
	"time"

	"github.com/dbarena/dbarenactl/internal/sweepstate"
)

// EstimateRemaining estimates how long the rest of a sweep will take by
// simulating Step's launch policy forward from now: runs already in flight
// hold their slots until their expected finish, and new attempts start only
// when a slot is free and no other provision is in flight, in the order
// nextToLaunch picks them. Every run is assumed to take its test point's
// average successful duration and to succeed.
//
// ok is false if a test point that still needs runs, or that has a run in
// flight, has no successful run to estimate a duration from.
func EstimateRemaining(testPoints []*sweepstate.TestPoint, runs []*sweepstate.Run, now time.Time, maxConcurrency int) (remaining time.Duration, ok bool) {
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}
	avg := AvgSuccessDurations(runs)
	provision := avgProvisionDuration(runs)

	// Simulate on copies so counting simulated successes leaves the caller's
	// test points alone.
	tps := make([]*sweepstate.TestPoint, len(testPoints))
	byID := make(map[string]*sweepstate.TestPoint, len(testPoints))
	for i, tp := range testPoints {
		c := *tp
		tps[i] = &c
		byID[c.ID] = &c
	}
	launchable, _, _ := classifyTestPoints(tps)
	for _, tp := range launchable {
		if _, ok := avg[tp.ID]; !ok {
			return 0, false
		}
	}

	// A simulated run in flight: its test point and when it finishes,
	// relative to now.
	type slot struct {
		testPointID string
		finish      time.Duration
	}
	var running []slot
	pending := map[string]int{}
	// launcherFreeAt is when the current provision (if any) finishes,
	// relative to now.
	var launcherFreeAt time.Duration
	for _, r := range runs {
		if r.Status.Terminal() {
			continue
		}
		d, ok := avg[r.TestPointID]
		if !ok {
			return 0, false
		}
		elapsed := now.Sub(r.CreatedAt)
		running = append(running, slot{r.TestPointID, max(d-elapsed, 0)})
		pending[r.TestPointID]++
		if r.Status == sweepstate.RunLaunching {
			launcherFreeAt = max(launcherFreeAt, provision-elapsed)
		}
	}

	var t, end time.Duration
	for {
		for t >= launcherFreeAt && canLaunch(len(running), maxConcurrency, false) {
			launchable, _, _ := classifyTestPoints(tps)
			next := nextToLaunch(launchable, pending)
			if next == nil {
				break
			}
			pending[next.ID]++
			running = append(running, slot{next.ID, t + avg[next.ID]})
			launcherFreeAt = t + provision
		}
		if len(running) == 0 {
			return end, true
		}

		// Advance to the next event: the earliest finish, or the launcher
		// freeing up while a slot is free.
		sort.Slice(running, func(i, j int) bool { return running[i].finish < running[j].finish })
		next := running[0].finish
		if launcherFreeAt > t && launcherFreeAt < next && len(running) < maxConcurrency {
			next = launcherFreeAt
		}
		t = next
		for len(running) > 0 && running[0].finish <= t {
			done := running[0]
			running = running[1:]
			pending[done.testPointID]--
			if tp, ok := byID[done.testPointID]; ok {
				tp.SuccessesCount++
			}
			end = max(end, done.finish)
		}
	}
}

// AvgSuccessDurations returns each test point's average duration across its
// successful runs, keyed by test point id, for test points with at least one
// such run. A run's duration spans launch to finalization, so it includes
// provisioning, the workload, fetching results and teardown.
func AvgSuccessDurations(runs []*sweepstate.Run) map[string]time.Duration {
	sums := map[string]time.Duration{}
	counts := map[string]int{}
	for _, r := range runs {
		if r.Outcome == "success" && r.Status.Terminal() {
			sums[r.TestPointID] += r.UpdatedAt.Sub(r.CreatedAt)
			counts[r.TestPointID]++
		}
	}
	avgs := make(map[string]time.Duration, len(sums))
	for id, sum := range sums {
		avgs[id] = sum / time.Duration(counts[id])
	}
	return avgs
}

// avgProvisionDuration returns the average time from launch to hand-off
// across runs that recorded it, or 0 if none did.
func avgProvisionDuration(runs []*sweepstate.Run) time.Duration {
	var sum time.Duration
	var n int
	for _, r := range runs {
		if r.ProvisionedAt != nil {
			sum += r.ProvisionedAt.Sub(r.CreatedAt)
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / time.Duration(n)
}

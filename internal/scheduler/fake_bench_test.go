package scheduler

import (
	"context"
	"errors"
	"sync"

	"github.com/dbarena/dbarenactl/internal/bench"
)

// fakeRun is one run's controllable simulated state within fakeBench.
type fakeRun struct {
	state          *bench.RunState
	notFound       bool
	launchErr      error
	statusErr      error
	fetchErr       error
	fetchFailCount int // Fetch fails this many times before succeeding/using fetchErr
	teardownErr    error
}

// fakeBench is an in-memory bench.Runner double. Tests drive a run's
// lifecycle across successive Step calls by mutating the fakeRun returned
// from runs(runID) between calls -- mirroring how a real run's state
// changes on benchctl's side between polls.
type fakeBench struct {
	mu            sync.Mutex
	runsByID      map[string]*fakeRun
	launchCalls   []string
	fetchCalls    []string
	teardownCalls []string
	onLaunch      func(runID string)
}

func newFakeBench() *fakeBench {
	return &fakeBench{runsByID: map[string]*fakeRun{}}
}

// run returns (creating if needed) the fakeRun for runID, for tests to
// configure before the next Step call observes it.
func (f *fakeBench) run(runID string) *fakeRun {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runsByID[runID]
	if !ok {
		r = &fakeRun{state: &bench.RunState{RunID: runID, Phases: map[string]string{}}}
		f.runsByID[runID] = r
	}
	return r
}

func (f *fakeBench) LaunchAsync(_ context.Context, runID, _ string, _ map[string]string) error {
	f.mu.Lock()
	f.launchCalls = append(f.launchCalls, runID)
	if _, ok := f.runsByID[runID]; !ok {
		f.runsByID[runID] = &fakeRun{state: &bench.RunState{RunID: runID, Phases: map[string]string{}}}
	}
	onLaunch := f.onLaunch
	f.mu.Unlock()

	// Run the hook (which may itself call f.run(runID) to configure the
	// just-created entry, e.g. setting launchErr) before deciding the
	// outcome, so tests can react to a launch as it happens rather than
	// needing to pre-seed state under a not-yet-known run id.
	if onLaunch != nil {
		onLaunch(runID)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runsByID[runID].launchErr
}

func (f *fakeBench) Status(_ context.Context, runID string) (*bench.RunState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runsByID[runID]
	if !ok || r.notFound {
		return nil, bench.ErrRunNotFound
	}
	if r.statusErr != nil {
		return nil, r.statusErr
	}
	cp := *r.state
	phases := make(map[string]string, len(r.state.Phases))
	for k, v := range r.state.Phases {
		phases[k] = v
	}
	cp.Phases = phases
	return &cp, nil
}

func (f *fakeBench) Fetch(_ context.Context, runID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetchCalls = append(f.fetchCalls, runID)
	r, ok := f.runsByID[runID]
	if !ok {
		return errors.New("fake: fetch unknown run")
	}
	if r.fetchFailCount > 0 {
		r.fetchFailCount--
		return errors.New("fake: simulated fetch failure")
	}
	return r.fetchErr
}

func (f *fakeBench) Teardown(_ context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.teardownCalls = append(f.teardownCalls, runID)
	r, ok := f.runsByID[runID]
	if !ok {
		return nil
	}
	return r.teardownErr
}

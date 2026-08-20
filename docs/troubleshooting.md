# Troubleshooting

This document covers common scenarios when `dbarenactl` sweep stops prematurely, 
how to resume and how to ensure all infrastructure has been correctly torn down. 

We assume you're already familiar with `benchctl` itself so refer to its documentation
for details.

## How a sweep runs

For each test point in a sweep, `dbarenactl` delegates to `benchctl` to set up an environment
(blocking until it's ready) and lets the benchmark run in the background. Once a benchmark
is completed, `dbarenactl` pulls the results and tears the environment down (blocking). 
Several test points can be in flight at once, up to a concurrency limit.

Importantly, `dbarenactl` completely relies on `benchctl` for the low-level plumbing.
If you're unsure about `dbarenactl` reporting, use `benchctl` directly to investigate.

## When a sweep stops on its own

A sweep can stop prematurely for several reasons. Below we discuss common situations,
their resolution and how to ensure there is no dangling infrastructure. 

### A benchmark exhausts its retry budget / control machine reboots

**Situation**: every test point gets a budget of allowed failures (control 
via `--max-workload-failures`). Once any test point uses up its budget, the *entire*
sweep stops immediately, even if other test points still have benchmarks running. 
Note that already running benchmarks will not be actively stopped but continue to
run in the background.

This scenario also applies when the machine that runs `dbarenactl` reboots in the
middle of a sweep.

**How to resolve:** 

1. Investigate why a test point has failed. Inspect `dbarenactl`'s logs for the sweep
   in `~/.dbarenactl/sweeps/$SWEEP_ID/logs` and eliminate the root cause (e.g. quota issue).
2. Run `dbarenactl resume`.


When resuming you have two choices:

- **Continue**: keeps every result gathered so far, and picks up again at the 
  failed test point with a fresh error budget. Any benchmarks that were in progress
  continue normally and are reconciled the next time `dbarenactl` polls status.
- **Start fresh**: discard all prior results and start from scratch. Before the restart,
  it tears down every environment that's still active as per `dbarenactl`'s state tracking.

**How to identify dangling infrastructure**: `dbarenactl status <sweep-id>` lists every run
it still considers active. For each one, run `benchctl status <run-id>` (or
`benchctl connect <run-id> driver` to connect to the load test machine. Tear down
any infrastructure manually via `benchctl teardown <run-id>`.

### An environment for a test point never started

**Situation**: if `benchctl` fails while setting up a test point's environment, the
sweep stops right away. Unlike a failed benchmark, this is treated as a permanent error.

**How to resolve:** 

1. Analyze why `benchctl` couldn't set up the environment (check the logs in
   `~/.dbarenactl/sweeps/$SWEEP_ID/logs`).
2. Run `dbarenactl resume`. As the failure happened in environment bootstrap there is 
   no dangling infrastructure to tear down.

### A benchmark finished, but pulling its results keeps failing

**Situation**: `dbarenactl` retries to pull results several times. If it still can't 
get the results, it stops the sweep. The environment for that run is kept up. 

**How to resolve**: figure out what's blocking the pull, then run `dbarenactl resume`. 
You can also pull results manually with `benchctl fetch <run-id> --dest <path>`.

**How to identify dangling infrastructure**: this environment has intentionally *not* been
torn down yet. Check `benchctl status <run-id>` (or `connect`) to confirm it's still
there for as long as it takes to get the results pulled. Tear down manually if needed
via `benchctl teardown <run-id>`

### A benchmark finished and its results were pulled, but teardown failed

**Situation**: `dbarenactl` tried to tear an environment down, `benchctl` reported a
problem doing so (e.g. credentials expired). The sweep stops and the environment
will stay up and running.

**How to resolve**: 

1. Identify and eliminate the root cause of the failure. 
2. Run `dbarenactl resume`, which will retry the teardown.

**How to identify dangling infrastructure**: Check `benchctl status <run-id>` first.
If the environment is still there and you don't want to wait for a
resume, tear it down manually with `benchctl teardown <run-id>`.

## What dbarenactl can't detect

If the remote benchmark infrastructure dies without `benchctl` reporting it as a failure,
`dbarenactl` cannot notice on its own and it never tears down any infrastructure. This 
is relevant on resume: choosing "continue" never touches a run that's already being
tracked as in progress, since the whole point is to leave ongoing work alone. If you 
suspect this has happened, check the run directly with `benchctl status <run-id>` or
`benchctl connect <run-id> driver`.

If a benchctl run is truly stuck, tear it down yourself with `benchctl teardown <run-id>`.
`dbarenactl` periodically polls status and notices the environment is gone. It then
finalizes the run as failed itself, charging its test point's failure budget and freeing
its slot for a fresh attempt.

## Quick checklist

When a sweep has stopped and you want to be sure nothing was left behind:

1. `dbarenactl status <sweep-id>` to see overall sweep status.
2. For each active run listed, issue `benchctl status <run-id>` for details.
3. Manually tear down infrastructure with: `benchctl teardown <run-id>`.
4. Consider the sweep's infrastructure town down only once every run it listed is 
   accounted for.

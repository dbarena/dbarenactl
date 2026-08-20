# dbarenactl

Sweep orchestrator for the public dbarena benchmarks. It uses a manifest file to configure test points, uses `benchctl` to execute them and keeps track of progress.

## Prerequisites

**benchctl**, built and reachable on `PATH` (or pointed at via `--benchctl-bin`).

## Install

**Build from source** (requires [mise](https://mise.jdx.dev/)):

```bash
git clone git@github.com:dbarena/dbarenactl.git
cd dbarenactl
mise trust
mise run build
```

## Quick start

```bash
# Preview what a sweep would do, without touching anything
./dbarenactl run --candidate candidates/smoke-test.yaml --dry-run

# Run it
./dbarenactl run --candidate candidates/smoke-test.yaml
```

`dbarenactl` keeps its state (a local database, per-sweep locks, logs, and fetched result
files) under `~/.dbarenactl`. Override with `DBARENACTL_HOME` to run isolated instances on
one machine.

## Candidate manifests

A candidate manifest is a YAML file describing one sweep: the provider and workload under
test, the `benchctl` scenario to run, and the list of test points (tier, bound type, and
scenario parameters) to sweep through. See [candidates/](candidates/) for examples, e.g.
[candidates/smoke-test.yaml](candidates/smoke-test.yaml):

```yaml
provider: supabase
workload: tpcc
scenario_path: ../../benchctl/scenarios/oriole-vs-postgres-tpcc-ec2.yaml
test_points:
  - tier: small
    bound_type: io
    set:
      warehouses: "1"
      threads: "1"
      duration: "1m"
```

For each test point, `dbarenactl` calls `benchctl` to provision an environment, runs the
workload for the required number of successful iterations, pulls results, and tears the
environment down. Up to `--max-concurrency` iterations can run at once in total, spread
across test points and/or stacked concurrently on the same test point -- whichever still
needs successes.

## Monitoring and resuming

```bash
# List sweeps that haven't finished yet
./dbarenactl status

# Show detail for one sweep
./dbarenactl status <sweep-id>

# Continue a sweep that stopped (e.g. after exhausting its failure budget)
./dbarenactl resume <sweep-id>
```

If a sweep stops because a test point ran out of retries, `resume` asks whether to keep
existing results and extend the budget, or discard progress and start fresh. See
[docs/troubleshooting.md](docs/troubleshooting.md) for how sweeps fail and how to check for
infrastructure left running.

## Command reference

| Command | Description |
|---|---|
| `dbarenactl run --candidate <manifest>` | Run a candidate manifest's sweep end to end |
| `dbarenactl run --candidate <manifest> --dry-run` | Preview the benchctl invocations without running anything |
| `dbarenactl resume [sweep-id]` | Continue an incomplete sweep, or list incomplete sweeps if no id is given |
| `dbarenactl status [sweep-id]` | Show sweep progress, or list incomplete sweeps if no id is given |
| `dbarenactl results <sweep-id>` | Assemble a results file from a sweep's artifacts (not yet implemented) |
| `dbarenactl pricing fetch\|set <provider>` | Record a provider pricing snapshot (not yet implemented) |

Common `run` flags: `--max-concurrency` (default 1), `--iterations` (successful runs
required per test point, default 3), `--on-workload-failure retry|fail-teardown`, `--set
key=value` to override manifest parameters, `--benchctl-bin` to point at a non-default
`benchctl` binary.

## Development

```bash
mise trust          # allow mise to read mise.toml
mise install        # install Go
mise run build      # build ./dbarenactl
mise run test       # run the unit test suite
mise run test-e2e   # run end-to-end tests against a fake benchctl binary
mise run lint       # gofmt + go vet
mise run format     # format Go files
```

Cross-compilation:

```bash
mise run build-linux   # linux/amd64 + linux/arm64 → ./bin/
mise run build-darwin  # darwin/amd64 + darwin/arm64 → ./bin/
mise run build-all     # all four platforms → ./bin/
```

## Further reading

- [Troubleshooting guide](docs/troubleshooting.md)

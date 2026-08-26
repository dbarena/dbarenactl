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

A candidate manifest is a YAML file describing one sweep: the provider/product/plan and
workload under test, the `benchctl` scenario to run, and the list of test points (tier, bound
type, and scenario parameters) to sweep through. `provider` is one of `AWS`, `GCP`, `Supabase`;
`product` is the one managed database product valid for that provider (`RDS`, `Cloud SQL for
Postgres`, `Supabase`); `plan` selects an edition/tier where the product has one (GCP:
`Enterprise` or `Enterprise Plus`; Supabase: always `Pro`; AWS has none today). See
[candidates/](candidates/) for examples, e.g. [candidates/smoke-test.yaml](candidates/smoke-test.yaml):

```yaml
provider: Supabase
product: Supabase
plan: Pro
workload: tpcc
region: us-east-1
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

# Delete an obsolete sweep (also tears down infrastructure)
./dbarenactl delete <sweep-id>
```

If a sweep stops because a test point ran out of retries, `resume` asks whether to keep
existing results and extend the budget, or discard progress and start fresh. See
[docs/troubleshooting.md](docs/troubleshooting.md) for how sweeps fail and how to check for
infrastructure left running.

## Pricing

`dbarenactl pricing` fetches or manually records provider on-demand list pricing (never
discounted) and caches it locally only, alongside the rest of dbarenactl's state -- nothing
is ever uploaded anywhere. Every `fetch`/`set` call appends a new immutable snapshot rather
than overwriting the previous one, so pricing history is preserved; each snapshot records
both when dbarenactl captured it and, if the provider's source exposes one, when the
provider itself last changed the price. Region is never typed by hand: `fetch`/`set` take
`--candidate <manifest>` and derive both the provider and the region from that manifest's
`provider:`/`region:` fields, so a snapshot always reflects the same region a candidate was
actually run against.

```bash
# Fetch aws/rds pricing for the region candidates/aws-rds-tpcc.yaml declares
./dbarenactl pricing fetch --candidate candidates/aws-rds-tpcc.yaml

# supabase has no public pricing API, but does publish an agent-facing pricing
# doc (supabase.com/pricing.md) that's fetched and parsed the same way
./dbarenactl pricing fetch --candidate candidates/supabase-tpcc.yaml

# for a provider with neither, record a snapshot by hand instead
./dbarenactl pricing set --candidate candidates/supabase-tpcc.yaml --file supabase-prices.json

# See what's cached, and inspect one snapshot's line items
./dbarenactl pricing list
./dbarenactl pricing show aws/rds
```

## Command reference

| Command | Description |
|---|---|
| `dbarenactl run --candidate <manifest>` | Run a candidate manifest's sweep end to end |
| `dbarenactl run --candidate <manifest> --dry-run` | Preview the benchctl invocations without running anything |
| `dbarenactl resume [sweep-id]` | Continue an incomplete sweep, or list incomplete sweeps if no id is given |
| `dbarenactl status [sweep-id]` | Show sweep progress, or list incomplete sweeps if no id is given |
| `dbarenactl delete <sweep-id>` | Permanently delete a sweep's state and tear down associated infrastructure |
| `dbarenactl results <sweep-id>` | Assemble a results file from a sweep's artifacts (not yet implemented) |
| `dbarenactl pricing fetch --candidate <manifest>` | Fetch and record a pricing snapshot from a provider's own primary source (aws/rds, gcp/cloudsql, gcp/cloudsql-enterprise-plus, supabase); provider and region are derived from the manifest |
| `dbarenactl pricing set --candidate <manifest> --file <items.json>` | Manually record a pricing snapshot for a provider with no automated source |
| `dbarenactl pricing list [--provider <p>] [--all]` | List cached pricing snapshots (latest per provider/region by default, `--all` for full history) |
| `dbarenactl pricing show <provider> [--region <r>]` | Show a snapshot's line items in full |

Common `run` flags: `--max-concurrency` (default 1), `--iterations` (successful runs
required per test point, default 3), `--on-workload-failure retry|fail-teardown`, `--set
key=value` to override manifest parameters, `--benchctl-bin` to point at a non-default
`benchctl` binary.

Common `delete` flags: `--yes`/`-y` to skip the confirmation prompt, `--benchctl-bin` to
point at a non-default `benchctl` binary.

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

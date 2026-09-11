# dbarenactl

Sweep orchestrator for the dbarena benchmarks. It uses a manifest file to configure test points, uses `benchctl` to execute them and keeps track of progress.

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
./dbarenactl run --candidate candidates/aws-rds-tpcc.yaml --dry-run

# Run it
./dbarenactl run --candidate candidates/aws-rds-tpcc.yaml
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
`Enterprise`; Supabase: always `Pro`; AWS has none today). 
See [candidates/](candidates/) for examples.

For each test point, `dbarenactl` calls `benchctl` to provision an environment, runs the
workload for the required number of successful iterations, pulls results, and tears the
environment down. Up to `--max-concurrency` iterations can run at once in total, spread
across test points and/or stacked concurrently on the same test point.

## Monitoring and resuming

```bash
# List sweeps that haven't finished yet, most recently started first
./dbarenactl status

# Include completed sweeps too
./dbarenactl status --all

# Show detail for one sweep
./dbarenactl status <sweep-id>

# Continue a sweep that stopped (e.g. after exhausting its failure budget)
./dbarenactl resume <sweep-id>

# Delete an obsolete sweep (also tears down infrastructure)
./dbarenactl delete <sweep-id>
```

Each listed sweep shows a `STARTED` timestamp: the initial run, or the most recent resume,
whichever is later.

If a sweep stops because a test point ran out of retries, `resume` asks whether to keep
existing results and extend the budget, or discard progress and start fresh. See
[docs/troubleshooting.md](docs/troubleshooting.md) for how sweeps fail and how to check for
infrastructure left running.

## Pricing

`dbarenactl pricing` fetches or manually records provider on-demand list pricing and caches 
it locally, alongside the rest of dbarenactl's state. Every `fetch`/`set` call appends a new
immutable snapshot, so pricing history is preserved; each snapshot records both when dbarenactl
captured it and, if the provider's source exposes one, when the provider last changed the price.
`fetch`/`set` take `--candidate <manifest>` and derive both the provider and the region from
that manifest's `provider:`/`region:` fields, so a snapshot always reflects the same region a
candidate was actually run against.

```bash
# Fetch aws/rds pricing for the region candidates/aws-rds-tpcc.yaml declares
./dbarenactl pricing fetch --candidate candidates/aws-rds-tpcc.yaml

# supabase has no public pricing API, but does publish an agent-facing pricing
# doc (supabase.com/pricing.md) that's fetched and parsed the same way
./dbarenactl pricing fetch --candidate candidates/supabase-tpcc.yaml

# for a provider with neither, record a snapshot by hand instead
./dbarenactl pricing set --candidate candidates/supabase-tpcc.yaml --file supabase-prices.json

# Check price snapshots, and inspect one snapshot's line items
./dbarenactl pricing list
./dbarenactl pricing show aws/rds
```

## Results

`dbarenactl results <sweep-id>` assembles one `result.json` per test point that can be
submitted as a PR to `dbarena/dbarena`. For each test point it picks the one successful iteration
whose peak-concurrency throughput is the median among that test point's iterations, and
reports every field from that run alone. Instance sizing is read from the candidate manifest's
`set:` block (`disk_size_gb`, `disk_iops`, `disk_throughput_mibps`) and its `pricing:` block
(`db_instance_type`, `disk_type`, and, for AWS tiers that cross a storage-baseline threshold,
`disk_baseline_iops`/`disk_baseline_throughput_mibps`) -- `pricing:` is dbarenactl-internal
metadata that this command's cost calculators need. All data are re-read from the manifest fresh on
every `results` invocation, so re-running `results` after a manifest correction always reflects the
corrected values, even for a sweep that ran before the correction. If a pricing snapshot is cached
for the sweep's provider/product/plan/region, its monthly cost is computed and included (with a
full per-component breakdown logged to `~/.dbarenactl/sweeps/<sweep-id>/logs/pricing-audit.log` for
review before publishing); otherwise the result is still written, with `pricing: null` and a
warning.

```bash
# Fetch pricing first (optional, but needed for cost data in the output)
./dbarenactl pricing fetch --candidate candidates/aws-rds-tpcc.yaml

# Write results/<provider>/<workload>/<scenario>/result.json for every test point
# that reached its required number of successful iterations
./dbarenactl results aws-rds-tpcc-2ed91ebe843e

# From a dbarena checkout (or a directory with one as a sibling), results/index.json
# is updated automatically. Otherwise, --dest picks the destination explicitly.
./dbarenactl results aws-rds-tpcc-2ed91ebe843e --dest ../dbarena

# Emit a result even for a test point short of its required iterations
./dbarenactl results aws-rds-tpcc-2ed91ebe843e --force
```

## Command reference

| Command | Description |
|---|---|
| `dbarenactl run --candidate <manifest>` | Run a candidate manifest's sweep end to end |
| `dbarenactl run --candidate <manifest> --dry-run` | Preview the benchctl invocations without running anything |
| `dbarenactl resume [sweep-id]` | Continue an incomplete sweep, or list incomplete sweeps if no id is given |
| `dbarenactl status [sweep-id] [--all]` | Show sweep progress, or list incomplete sweeps (or all sweeps with `--all`) if no id is given |
| `dbarenactl delete <sweep-id>` | Permanently delete a sweep's state and tear down associated infrastructure |
| `dbarenactl results <sweep-id> [--dest <dir>] [--candidate <manifest>] [--force]` | Assemble `result.json` files from a sweep's fetched artifacts, ready for a `dbarena/dbarena` PR |
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

### Use this checkout from anywhere

```bash
source scripts/use.sh   # or: . scripts/use.sh
```

- **Ensure to *source* the script, not execute it.**
- It only affects your *current* shell session. To persist it across shell
  sessions, add it to your shell's rc file.
- After sourcing, `dbarenactl` resolves to a wrapper that rebuilds this
  checkout whenever the source has changed since the last build, so it
  always reflects current source, including uncommitted changes.

## Further reading

- [Troubleshooting guide](docs/troubleshooting.md)

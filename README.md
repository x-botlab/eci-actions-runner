# eci-actions-runner

Schedules GitHub Actions self-hosted runners on Alibaba Cloud ECI: one ephemeral
container group per job, registered with a one-shot JIT config, deleted when the
job ends.

```
GitHub Actions queue  ──►  eci-runner (reconcile loop)  ──►  ECI container group
      ▲                                                          │
      └──────────────── runner registers, runs 1 job, exits ◄────┘
```

GitHub owns the queue and ECI tags own the inventory, so a restart loses nothing:
the next reconcile reads both sides again.

## Layout

| Path | Purpose |
| --- | --- |
| [cmd/eci-runner/](cmd/eci-runner/) | entry point |
| [internal/config/](internal/config/) | configuration loading, validation and repo/pool/target resolution |
| [internal/pool/](internal/pool/) | pool definitions, sizing and label matching |
| [internal/github/](internal/github/) | App JWT, JIT configs, queued jobs, runner registry |
| [internal/eci/](internal/eci/) | ECI launch, describe and terminate behind `Backend` |
| [internal/scheduler/](internal/scheduler/) | reconcile loop |
| [internal/metrics/](internal/metrics/) | Prometheus series that stand in for a UI |
| [internal/cli/](internal/cli/) | subcommands |
| [pools.yaml](pools.yaml) | the configuration, reviewed like code |

## Shape of the configuration

Three collections, each a first-class entity:

- **installations → repos**: an App installation authorises a set of
  repositories. Tokens are cached per installation, so repos are grouped by the
  installation that can mint for them. A repository may appear under exactly one
  installation.
- **eci.targets**: one target is one region with its own switches, security group
  and instance role.
- **pools**: a pool lists one or more targets **in preference order**
  (`eci: [hangzhou, beijing]`), so a stock out or a throttle in the first zone
  falls through to the next one. `images` overrides the image per target,
  because a container registry endpoint is regional.

The scheduler then works on **bindings**, one per `(repo, pool)` pair — that unit
matters because a repository scoped runner can only serve its own repository. A
repo without a `pools` list receives every pool, so a new repo never silently
misses a pool that already exists.

Labels must be unique among the pools a repo binds, and no stricter: two repos
may reuse `eci-pool-short` for different shapes, since their runners never mix.

Only capacity failures (`StockExhausted`, quota, throttling) move on to the next
target. A bad credential, a bad parameter or a missing image stops the walk,
because it fails in every region and retrying just burns quota.

## Usage

```console
$ go build ./cmd/eci-runner
$ ./eci-runner --config pools.yaml config check
$ ./eci-runner --config pools.yaml repo ls
$ ./eci-runner --config pools.yaml pool ls
$ ./eci-runner --config pools.yaml server --metrics-addr :9090
```

`make check` runs gofmt, vet and the tests. `make docker` builds the image.

## Configuration

`pools.yaml` is validated on load, and unknown keys are rejected — a mistyped
label fails immediately instead of leaving jobs queued. Each pool maps one set of
runner labels onto one ECI shape, so `runs-on` in a workflow is matched by the
pool that declares that label:

```yaml
jobs:
  test:
    runs-on: [self-hosted, eci-pool-short]
```

`ttl_minutes` must stay above the job timeout; it is a backstop, never the reason
a healthy long job is killed.

Secrets stay out of the file: the App private key is mounted from a secret, and
the Alibaba Cloud credential comes from the instance RAM role.

## Status

Scaffold. The loop, CLI, configuration and metrics are wired; `Reconcile`,
`Launch`, `Describe`, `Terminate` and the GitHub calls still return
`not implemented`. Subcommands that need live state (`ps`, `gc`, `logs`,
`drain`) exit with the same marker.

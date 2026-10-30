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

## How one pass works

```
read the queue per repo          GitHub is the source of truth for demand
match jobs against each pool     runs-on is an AND over labels
read container groups            ECI is the source of truth for inventory
delete what can never run again  only terminal, stuck-pending or expired states
create the difference            clamp(queued + warm, min, max), budgeted per pass
```

Two rules keep this safe:

- **Never launch on partial information.** If a target's describe call fails, the
  binding is skipped for that pass instead of risking a doubled pool.
- **Never kill a healthy job.** A container group is only deleted when it cannot
  run again; the TTL is enforced by the platform through `ActiveDeadlineSeconds`,
  not by age-based cleanup. Long jobs survive arbitrarily long.

A runner created from a JIT config is not bound to a specific job: it registers
as ephemeral and takes whichever matching job arrives first. That is what makes a
`warm` instance possible, and it is also why the launch budget is capped by
`runner.max_concurrent_launches` rather than by the queue length alone.

Runners that registered but never came online are unregistered again, which is
what a container group that failed to start leaves behind.

## Verifying the App before the first launch

`github check` is read-only: it mints an installation token per repo, prints the
permissions GitHub actually granted, and exercises the two read paths. When
something is wrong it prints the one action that fixes it.

`github jit` proves the write path: it mints a JIT config per pool, then removes
the registration again. The encoded config is never printed. A pool that already
has jobs waiting is skipped, because a probe runner would claim real work and
then vanish with it.

```
REPO                         INSTALLATION  TOKEN EXPIRES  ACTIONS  ADMINISTRATION  QUEUED  RUNNERS  STATUS
x-botlab/eci-actions-runner  10000000      18:04          read     write           0       0        ok
```

## Usage

```console
$ go build ./cmd/eci-runner
$ ./eci-runner --config pools.yaml config check
$ ./eci-runner --config pools.yaml github check
$ ./eci-runner --config pools.yaml github jit
$ ./eci-runner --config pools.yaml repo ls
$ ./eci-runner --config pools.yaml pool ls
$ ./eci-runner --config pools.yaml ps
$ ./eci-runner --config pools.yaml gc              # dry run
$ ./eci-runner --config pools.yaml gc --apply      # delete
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

The App is identified by `github.app_id` (numeric) or `github.client_id` (OAuth
client ID). GitHub accepts either as the JWT issuer and now recommends the client
ID; both still require the private key, since the token must be RS256 signed.

## Status

Working: configuration, repo/pool/target resolution, the reconcile loop, ECI
launch/describe/delete with zone and region fallback, GitHub installation tokens,
JIT configs, queue reading, runner cleanup, metrics, and the `config`/`repo`/
`pool`/`ps`/`gc` verbs.

Still missing: the `workflow_job` webhook fast path (polling already covers
correctness, only latency is affected), `logs` (needs the log pipeline) and
`drain` (needs a runtime override that does not require a new commit).

## Before pointing a real repository at it

- Image: the container starts `/home/runner/run.sh` with the JIT config in
  `ACTIONS_RUNNER_INPUT_JITCONFIG`. Use an image that also passes
  `--disableupdate` at configure time, or a runner release will try to self update
  inside every container.
- Registry: pull from the ACR instance in the same region as the target, and use
  its VPC endpoint, otherwise image traffic crosses the internet.
- Network: NAT gateway for egress, no inbound rules, and a fixed egress IP so the
  dependency mirrors can be allow-listed.
- Quota: ECI vCPU quota is per region and starts low. Raise it before the first
  busy week.

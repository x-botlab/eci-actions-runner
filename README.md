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
| [image/](image/) | the runner container image and how to rebuild it |
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

The App is identified by `github.client_id`, the OAuth client ID that GitHub
recommends as the JWT issuer. It is a public identifier; the App private key is
what proves identity, since the token must be RS256 signed.

Private images need a registry credential, and ECI matches it to the image by
server. The password is read from the environment at launch time, so the file
stays committable and a diagnostic verb (`ps`, `gc`) still works when the
registry password has rotated:

```yaml
eci:
  registries:
    - server: registry.cn-hangzhou.aliyuncs.com
      username_env: ECI_ACR_USERNAME
      password_env: ECI_ACR_PASSWORD
```

`server` subcommand validates those variables at start-up, so a missing one stops
the process immediately instead of failing the first launch.

A runner needs egress to reach GitHub, so a target either sits behind a NAT
gateway or sets `auto_create_eip: true` and gives every container group its own
public IP:

```yaml
eci:
  targets:
    - name: hongkong
      region_id: cn-hongkong
      vswitch_ids: [vsw-xxx]
      security_group_id: sg-xxx
      auto_create_eip: true
      eip_bandwidth: 20
```

## Verifying a deployment without a workflow

```console
$ eci-runner --config pools.yaml github check          # App permissions, read-only
$ eci-runner --config pools.yaml github jit            # mint and delete a registration
$ eci-runner --config pools.yaml eci probe --pool pool-short
```

`eci probe` runs the production launch path once and reports each phase:

```
jit config      eci-pool-short-probe-4d97bc (4120 bytes, sha256:912927b6 in 2.6s)
created         eci-j6cgd9jq3tmgyyaqc5zq in target hongkong after 4.6s
status          Scheduling
public ip       8.210.164.137
status          Running
cold start      21s (create 5s, image pull and boot 16s)
runner offline  eci-pool-short-probe-4d97bc
runner online   eci-pool-short-probe-4d97bc
unregistered    runner 6
deleted         eci-j6cgd9jq3tmgyyaqc5zq
```

`runner online` is the line that matters: it means the image pulled, the JIT
config was accepted, and the container group can reach github.com. Everything
before it only proves the container booted.

## Status

Working: configuration, repo/pool/target resolution, the reconcile loop, ECI
launch/describe/delete with zone and region fallback, GitHub installation tokens,
JIT configs, queue reading, runner cleanup, metrics, and the `config`/`repo`/
`pool`/`ps`/`gc` verbs.

Still missing: the `workflow_job` webhook fast path (polling already covers
correctness, only latency is affected), `logs` (needs the log pipeline) and
`drain` (needs a runtime override that does not require a new commit).

## The runner image

[image/](image/) builds and publishes the container a job runs in:

```
registry.cn-hangzhou.aliyuncs.com/xbotlab/actions-runner:2.338.2
```

`make image-push` builds and pushes it. The runner version is pinned and must be
bumped when GitHub releases a new runner, because a JIT runner cannot disable its
own auto-update; see [image/README.md](image/README.md) for the details and the
measured layer sizes.

## Before pointing a real repository at it

- Image: the container reads its one-shot registration from
  `ACTIONS_RUNNER_INPUT_JITCONFIG`; [image/entrypoint.sh](image/entrypoint.sh)
  handles that and nothing else.
- Registry: pull from the ACR instance in the same region as the target, and use
  its VPC endpoint, otherwise image traffic crosses the internet.
- Network: NAT gateway for egress, no inbound rules, and a fixed egress IP so the
  dependency mirrors can be allow-listed.
- Quota: ECI vCPU quota is per region and starts low. Raise it before the first
  busy week.

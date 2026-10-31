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
read the runner registry         GitHub knows which of them are idle
delete what can never run again  only terminal, stuck-pending or expired states
create the shortfall             clamp(queued + warm, min, max) over *usable* capacity
```

Capacity is not a count of machines. A container group in the `Running` state can
be idle, busy with a job, or already finished — its runner unregisters itself
before the group is collected — and only the first can start a queued job. The two
sides are joined by name, which one launch gives both of them:

| container group | runner registry | bucket |
| --- | --- | --- |
| `Pending` / `Scheduling` | (registered already, not connected) | starting |
| `Running` | `online`, not busy | **idle** |
| `Running` | `online`, busy | occupied |
| `Running` | missing | exiting |
| (not listed yet) | — | starting |

The plan is therefore `queued + warm` minus `idle + starting`, and `max` caps the
total of all four buckets, because an occupied instance costs the same as an idle
one. Counting running groups instead made a full pool look big enough: a deploy
once waited 5m22s behind four unrelated test jobs on the same pool.

Two rules keep this safe:

- **Never launch on partial information.** If a target's describe call or the
  runner listing fails, the binding is skipped for that pass instead of risking a
  doubled pool.
- **Never kill a healthy job.** A container group is only deleted when it cannot
  run again; the TTL is enforced by the platform through `ActiveDeadlineSeconds`,
  not by age-based cleanup. Long jobs survive arbitrarily long.

A runner created from a JIT config is not bound to a specific job: it registers
as ephemeral and takes whichever matching job arrives first. That is what makes a
`warm` instance possible, and it is also why the launch budget is capped by
`runner.max_concurrent_launches` rather than by the queue length alone.

Runners that registered but never came online are unregistered again, which is
what a container group that failed to start leaves behind.

The describe call is the one step where a wrong wire format bites hard:
`DescribeContainerGroups` takes `ContainerGroupIds` as a **JSON array string**,
not a comma separated list. A comma list is accepted while the pool holds a
single container group and rejected with `400 InvalidParameter` from the second
one on — and because a failed describe skips the binding, the pool stops
scaling *and* stops collecting itself. Encode it with `encoding/json`.

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
      eip_bandwidth: 200   # Mbit/s
      eip_isp: BGP_PRO     # premium line; BGP is the default
```

`eip_bandwidth` and `eip_isp` without `auto_create_eip` are rejected, because the
setting would silently do nothing. Both were verified against the created
resource, not just accepted by the API: it reports `ISP=BGP_PRO`,
`Bandwidth=200`, `InternetChargeType=PayByTraffic`.

## Docker in a job

A job that runs `docker build` needs a daemon, and an ECI container group has no
socket to mount: it is one container. So the daemon runs inside the runner
container, started by the image's entry point and supervised next to the runner.
The pool asks for it with one flag:

```yaml
pools:
  - name: pool-docker
    privileged: true
    ...
```

`privileged: true` does two things, both required by ECI: it creates the
container group privileged — the account has to be on ECI's privileged-container
whitelist, otherwise `CreateContainerGroup` is refused — and passes
`ECI_RUNNER_PRIVILEGED=1`, which is how the image knows to start a daemon.
`dockerd --group runner` makes the socket usable by the uid the job steps run as,
so a step calls `docker` without `sudo`.

Why not a sidecar: a container group is only terminal when every container in it
has exited, and only a terminal group is deleted. A daemon in a second container
never exits, so the group would look `Running` for the rest of its `ttl_minutes`
— an eight-minute job would hold an instance for six hours. The daemon therefore
runs inside the runner container, started by the job hook
([image/job-started.sh](image/job-started.sh)) just before the job's containers
are created.

That hook is the whole lifecycle: the runner is the container's only process, so
the group ends when the job does, and no supervisor sits between the runner and
the scheduler. A job that fails makes the container exit non-zero, which the
scheduler sees; the hook itself never fails a job — if the daemon does not come
up, the job's own `docker` commands report the real error.

A privileged pool also gets an EmptyDir volume at `/var/lib/docker`, because the
daemon cannot store images on ECI's own filesystem: the container root is
overlayfs, overlayfs cannot stack on itself, and every nested container dies with
`failed to mount … overlay … invalid argument`. The EmptyDir is ext3 on the
instance's ephemeral disk, where it works. Verified on ECI in cn-hongkong on an
8 vCPU / 16 GiB instance with a real privileged pool: `docker run --rm alpine:3`
and `docker build` with a `RUN` step both succeed, `dockerd` runs alongside the
runner in one container, and the socket is `root:runner` so a step calls `docker`
without `sudo`. ECI's cgroup v1 layout is writable and needs no fix.

Three consequences worth knowing:

- the steps inherit the privileged container. They already run with passwordless
  `sudo` inside a per-job container, so this is a drop of isolation inside the
  sandbox, not between jobs.
- an image pulled by the job's daemon dies with it: ECI's ImageCache covers the
  runner image, not what the nested daemon pulls, so those layers are cold in
  every job. The 30 GiB volume is also the ceiling for a job's image store.
- cold start grows: the image is ~170 MB larger, and a privileged group pulls it
  in about 80 s before the runner can register.

The daemon ships in the image, so a privileged pool needs the image rebuilt
(`make image-push`) before the flag means anything.

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

## Labels: a JIT runner gets only what you send

A normally registered runner receives `self-hosted`, `linux` and `x64` during
`config.sh`. A JIT registration has no such step, so those labels must be in the
API request or the runner comes online and stays idle while
`runs-on: [self-hosted, ...]` matches nothing. The scheduler always merges them
in, and a pool label may repeat them harmlessly.

The flip side is that `runs-on` is an AND: a job asking for a label no runner
carries stays queued rather than failing, so watch `eci_runner_queued_jobs`
rather than the job page.

One more field matters for cold start: `AutoMatchImageCache` is always sent, so a
container group reuses ECI's cached image layers when the pool has pulled the
same tag before.

## Probes

`--metrics-addr` also serves `/healthz` and `/readyz`. Readiness means a reconcile
pass has completed, so a rollout waits until GitHub and ECI have both answered once.
The probes live on the metrics port on purpose: the webhook port is the only one
that has to be reachable from the internet.

## The webhook fast path

Polling is correct on its own, but it costs up to one poll interval of latency.
`--webhook-addr` adds a `workflow_job` endpoint that cuts that:

```console
$ export GITHUB_WEBHOOK_SECRET=...      # the secret configured on the App
$ eci-runner --config pools.yaml server --webhook-addr :8080 --metrics-addr :9090
```

Point the App's webhook URL at `http://<host>:8080/webhook`, subscribe to
**Workflow job**, and set the same secret. Measured on a job that takes 14 seconds:

| | queue wait | job start after trigger |
| --- | --- | --- |
| polling only, 30s interval | 25.8s | 47s |
| webhook + polling | 9.4s | 34s |

The handler answers `202` before reconciling, because GitHub treats a response
slower than ten seconds as a failed delivery. What remains is not detection: it is
GitHub listing the run, one JIT mint, `CreateContainerGroup`, and the container
boot. A control plane running in the same region as the cluster is faster than
these numbers, which were measured across a long-haul link.

Two properties matter more than the numbers:

- the signature is mandatory. `--webhook-addr` without `GITHUB_WEBHOOK_SECRET`
  refuses to start, and an unsigned or tampered delivery gets a `401`.
- a delivery is only a hint. Losing one costs one poll interval, and the two
  follow-up passes exist because GitHub can deliver `queued` before the job is
  listable through the API.

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
registry.cn-hangzhou.aliyuncs.com/xbotlab/actions-runner:2.338.2-r2
```

`make image-push` builds and pushes it. The runner version is pinned and must be
bumped when GitHub releases a new runner, because a JIT runner cannot disable its
own auto-update; see [image/README.md](image/README.md) for the details and the
measured layer sizes.

## Before pointing a real repository at it

- Image: the container reads its one-shot registration from
  `ACTIONS_RUNNER_INPUT_JITCONFIG`; [image/runner.sh](image/runner.sh) is the
  container's only process, and a privileged pool additionally starts `dockerd`
  from the job hook.
- Registry: pull from the ACR instance in the same region as the target, and use
  its VPC endpoint, otherwise image traffic crosses the internet.
- Network: NAT gateway for egress, no inbound rules, and a fixed egress IP so the
  dependency mirrors can be allow-listed.
- Quota: ECI vCPU quota is per region and starts low. Raise it before the first
  busy week.

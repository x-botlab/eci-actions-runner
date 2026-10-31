# Runner image

The container a job runs in. Published to Alibaba Cloud Container Registry:

```
registry.cn-hangzhou.aliyuncs.com/xbotlab/actions-runner:<runner-version>
registry.cn-hangzhou.aliyuncs.com/xbotlab/actions-runner:latest
```

## Why build our own base

The official `ghcr.io/actions/actions-runner` image is 2.17 GB on its own because
it preinstalls toolchains this cluster does not use. Building on plain Ubuntu with
the official runner bundle drops the result to 2.56 GB — about the same total, but
every megabyte in it is a tool a job here actually calls. Measured layer sizes:

| Layer | Size |
| --- | --- |
| Ubuntu 24.04 | 117 MB |
| build/py/git toolchain (incl. iptables, nftables) | 527 MB |
| runner bundle (runner + its .NET runtime + action node runtimes) | 715 MB |
| Node 24 | 218 MB |
| Go | 296 MB |
| Docker CLI + buildx + daemon (+ runc shim) | 241 MB |

Pull time is the dominant part of the ECI cold start, so this image is meant to be
used together with an ImageCache and, for short jobs, a warm pool.

## Contents

| Tool | Version |
| --- | --- |
| Runner | 2.338.2 |
| Node | 24.22.2 (LTS, `npm`, `npx`, `corepack`) |
| Go | 1.27.3 |
| Python | 3.12.3 (`pip`, `venv`) |
| Docker CLI | 29.9.2 |
| dockerd | 29.9.2, started only for a privileged pool |
| buildx | 0.38.1 |
| plus | git + git-lfs, build-essential, jq, less, rsync, sudo, iptables |

The Docker CLI alone cannot build an image, so the daemon ships too: `dockerd`,
`runc`, `docker-proxy` and `docker-init` come from the same static tarball as the
CLI (`containerd` and `ctr` are left out, dockerd 29 uses the embedded
containerd). It is not started by default — only a pool with `privileged: true`
asks for it.

The daemon lives in this container rather than in a sidecar because an ECI
container group is one container and is only deleted once every container in it
has exited. A sidecar daemon never exits, so the group would stay `Running` until
its TTL even after the job ended.

[runner.sh](runner.sh) is the container's only process — PID 1 — so the group ends
as soon as the job does, with the runner's exit code. A privileged pool starts its
daemon from [job-started.sh](job-started.sh), the `ACTIONS_RUNNER_HOOK_JOB_STARTED`
hook, which runs before the runner creates the job's containers: `dockerd` is
detached with `setsid` so it outlives the hook, and `--group=runner` leaves a
socket the job steps can use without `sudo`. There is no supervisor in the image
for that reason — one less process to keep alive, and nothing masking the exit
code.

`PIP_BREAK_SYSTEM_PACKAGES=1` is set because Ubuntu 24.04 ships PEP 668 and a job
that runs `pip install -r requirements.txt` would otherwise fail outside a venv.

## The runner version must track GitHub releases

A JIT runner cannot switch off its own auto-update. `--disableupdate` exists only
for `config.sh`, and passing it to `run` fails outright:

```
Unrecognized command-line input arguments for command run: 'disableupdate'
```

No environment variable covers it either. So the only way to keep start-up free of
an update download is to ship the newest runner, which means rebuilding this image
when GitHub publishes one. `make image-versions` prints the upstream version
against the pinned one.

## How the credential reaches the runner

The scheduler sets `ACTIONS_RUNNER_INPUT_JITCONFIG` in the container environment
and passes no arguments. The runner reads that variable itself and then removes it
before starting a job:

```
INFO CommandSettings] Removing env var: 'ACTIONS_RUNNER_INPUT_JITCONFIG'
INFO CommandSettings] Env arg 'jitconfig': '***'
```

Two consequences worth keeping:

- the registration credential never appears in `ps`, because it is not an argument;
- job steps cannot read it, because the runner already deleted it.

[runner.sh](runner.sh) only guards that path: no JIT config and no arguments is a
configuration error (exit 64) rather than a container that hangs.

## Trying the privileged path locally

```
$ make image-build
$ docker run --rm --privileged -e ECI_RUNNER_PRIVILEGED=1 \
    -v /var/lib/docker actions-runner:dev
eci-runner: no JIT config in ACTIONS_RUNNER_INPUT_JITCONFIG ...
$ echo $?
64
```

The runner is the container's only process, so its exit code is the container's:
a job that fails makes the container exit non-zero, and the scheduler can tell a
broken start-up from a finished job. The daemon is not started here at all — only
the job hook does that, once a job is actually assigned.

`-v /var/lib/docker` is not decoration: without a real filesystem there, a nested
container fails with `failed to mount … overlay … invalid argument` when the
container's own root filesystem is overlayfs, which is the case both under Docker
Desktop and on ECI. The scheduler mounts an ECI EmptyDir at that path for a
privileged pool for exactly this reason.

## Build and push

```console
$ make image-build                      # local only
$ make image-push TAG=2.338.2           # build, tag latest, push both
```

Overrides: `REGISTRY`, `IMAGE`, `TAG`, `RUNNER_VERSION`, `NODE_VERSION`,
`GO_VERSION`, `APT_MIRROR`, `PLATFORM`.

On a network where `ghcr.io` or the default Ubuntu archive is slow, use a mirror:

```console
$ make image-build APT_MIRROR=http://mirrors.aliyun.com/ubuntu
```

`--provenance=false --sbom=false` is required when publishing: ACR rejects the
attestation manifest BuildKit adds by default with
`unknown manifest class for application/vnd.oci.empty.v1+json`.

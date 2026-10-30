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
| build/py/git toolchain | 518 MB |
| runner bundle (runner + its .NET runtime + action node runtimes) | 715 MB |
| Node 24 | 218 MB |
| Go | 296 MB |
| Docker CLI + buildx | 113 MB |

Pull time is the dominant part of the ECI cold start, so this image is meant to be
used together with an ImageCache and, for short jobs, a warm pool.

## Contents

| Tool | Version |
| --- | --- |
| Runner | 2.338.2 |
| Node | 24.22.2 (LTS, `npm`, `npx`, `corepack`) |
| Go | 1.27.3 |
| Python | 3.12.3 (`pip`, `venv`) |
| Docker CLI | 29.9.2 (no daemon) |
| buildx | 0.38.1 |
| plus | git + git-lfs, build-essential, jq, jq-less, rsync, sudo |

The Docker *daemon* is deliberately absent: `docker build` needs a daemon, and the
daemon belongs in a sidecar container in the same container group.

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

[entrypoint.sh](entrypoint.sh) only guards that path: no JIT config and no
arguments is a configuration error (exit 64) rather than a container that hangs.

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

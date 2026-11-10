# Runner image

The container a job runs in. Published to Alibaba Cloud Container Registry:

```
registry.cn-hangzhou.aliyuncs.com/xbotlab/actions-runner:<runner-version>
registry.cn-hangzhou.aliyuncs.com/xbotlab/actions-runner:latest
```

## Why build our own base

Building on Ubuntu 24.04 lets us choose the shared toolchains explicitly. The
image includes multiple PHP versions and common CLI tools so jobs do not need to
install them on every run. Specialized SDKs and browsers belong in separate pool
images. A local build with the default versions measured approximately 3.16 GB
uncompressed (`docker image inspect`); compressed registry transfer size differs.

Pull time is the dominant part of the ECI cold start, so this image is meant to be
used together with an ImageCache and, for short jobs, a warm pool.

## Contents

| Tool | Version |
| --- | --- |
| Runner | 2.338.2 |
| Node | 24.22.2 (LTS, `npm`, `npx`, `corepack`) |
| Go | 1.27.3 |
| PHP CLI | 8.2, 8.3, 8.4, 8.5; default 8.4 (Ondřej Surý PPA) |
| Composer | 2.11.1 (official PHAR, checksum verified) |
| Python | 3.12.3 (`pip`, `venv`) |
| uv / uvx | 0.13.7 |
| GitHub CLI | `gh` (APT package) |
| SSH client | `ssh`, `scp`, `sftp` (APT package) |
| Diagnostics | ripgrep (`rg`), `file`, dnsutils (`dig`, `nslookup`) |
| Docker CLI | 29.9.2 |
| dockerd | 29.9.2, started only for a privileged pool |
| buildx | 0.38.1 |
| plus | git + git-lfs, build-essential, jq, less, rsync, sudo, iptables |

### PHP versions and extensions

Each PHP version includes CLI and development tools (`phpize`, `php-config`),
bcmath, bz2, curl, GD, GMP, intl, LDAP, mbstring, MySQL, PostgreSQL, SQLite,
readline, SOAP, XML (including DOM, SimpleXML, XMLReader/Writer and XSL), ZIP,
APCu, Imagick and Redis. Common/built-in modules also provide ctype, exif,
fileinfo, OpenSSL, pcntl, PDO, sockets and tokenizer. No PHP web server or FPM
service is installed. Xdebug is left out to avoid overhead in ordinary jobs.

Use explicit binaries to run a matrix without changing system alternatives:

```sh
php8.2 /usr/local/bin/composer install --no-interaction --prefer-dist
php8.2 vendor/bin/phpunit
php8.5 /usr/local/bin/composer check-platform-reqs
```

To change the default for subsequent job steps, switch all three tools:

```sh
version=8.3
for tool in php phpize php-config; do
  sudo update-alternatives --set "$tool" "/usr/bin/${tool}${version}"
done
php --version
composer --version
```

The PHP versions use separate configuration directories under `/etc/php/<version>`.
PHP patch versions and APT tools follow the packages available at build time;
rebuild to pick up updates. `shivammathur/setup-php` may still access the network
for its setup scripts, extra extensions or tools; preinstallation does not make
that action offline. Builds verify the listed extensions on every PHP version.

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
`GO_VERSION`, `PHP_VERSIONS`, `PHP_DEFAULT_VERSION`, `COMPOSER_VERSION`,
`UV_VERSION`, `APT_MIRROR`, `PLATFORM`. `PHP_DEFAULT_VERSION` must be included in `PHP_VERSIONS`:

```sh
make image-build PHP_VERSIONS="8.3 8.4 8.5" PHP_DEFAULT_VERSION=8.4
```

To validate only the PHP and utility layers without downloading the runner,
Node, Go or Docker bundles:

```sh
docker build --target runner-tools -f image/Dockerfile -t actions-runner:tools image/
```

On a network where `ghcr.io` or the default Ubuntu archive is slow, use a mirror:

```console
$ make image-build APT_MIRROR=http://mirrors.aliyun.com/ubuntu
```

`--provenance=false --sbom=false` is required when publishing: ACR rejects the
attestation manifest BuildKit adds by default with
`unknown manifest class for application/vnd.oci.empty.v1+json`.

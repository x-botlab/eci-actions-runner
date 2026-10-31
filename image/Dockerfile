# syntax=docker/dockerfile:1

# A purpose-built runner image rather than an extension of the official one.
# The official image is 2.17 GB because it preinstalls toolchains this cluster
# does not use; building on plain Ubuntu keeps only what a job here actually
# needs, which directly shortens the ECI cold start.
#
# The runner version is pinned: a JIT runner cannot disable its own auto-update
# (verified: `run` rejects --disableupdate with "Unrecognized command-line input
# arguments for command run: 'disableupdate'"), so the image must always carry the
# newest release and has to be rebuilt when GitHub publishes one.
FROM ubuntu:24.04

ARG DEBIAN_FRONTEND=noninteractive
# Point this at a regional mirror when the default archive is slow, for example
# http://mirrors.aliyun.com/ubuntu
ARG APT_MIRROR=

ARG RUNNER_VERSION=2.338.2
ARG RUNNER_BASE_URL=https://github.com/actions/runner/releases/download
ARG NODE_VERSION=24.22.2
ARG NODE_BASE_URL=https://nodejs.org/dist
ARG GO_VERSION=1.27.3
ARG GO_BASE_URL=https://go.dev/dl
ARG DOCKER_VERSION=29.9.2
ARG DOCKER_BASE_URL=https://download.docker.com/linux/static/stable/x86_64
ARG BUILDX_VERSION=v0.38.1
ARG OSSUTIL_VERSION=2.3.0
ARG OSSUTIL_BASE_URL=https://gosspublic.alicdn.com/ossutil/v2

# Matches the official image's runner uid/gid convention.
ARG RUNNER_UID=1001
ARG RUNNER_GID=1001

LABEL org.opencontainers.image.title="eci-actions-runner" \
      org.opencontainers.image.description="GitHub Actions self-hosted runner for Alibaba Cloud ECI" \
      org.opencontainers.image.source="https://github.com/x-botlab/eci-actions-runner" \
      com.eci-actions-runner.runner-version="${RUNNER_VERSION}" \
      com.eci-actions-runner.node-version="${NODE_VERSION}" \
      com.eci-actions-runner.go-version="${GO_VERSION}"

# libicu is not optional: the listener is .NET and aborts at start-up without it.
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    set -eux; \
    if [ -n "${APT_MIRROR}" ]; then \
        for file in /etc/apt/sources.list /etc/apt/sources.list.d/ubuntu.sources; do \
            [ -f "$file" ] && sed -i \
                -e "s|http://archive.ubuntu.com/ubuntu|${APT_MIRROR}|g" \
                -e "s|http://security.ubuntu.com/ubuntu|${APT_MIRROR}|g" "$file" || true; \
        done; \
    fi; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        curl \
        git \
        git-lfs \
        iproute2 \
        iptables \
        jq \
        less \
        libffi-dev \
        libicu74 \
        libssl-dev \
        netcat-openbsd \
        nftables \
        pkg-config \
        procps \
        python3 \
        python3-dev \
        python3-pip \
        python3-venv \
        rsync \
        sudo \
        supervisor \
        tar \
        unzip \
        xz-utils \
        zip \
        zstd; \
    rm -rf /var/lib/apt/lists/*; \
    git lfs install --system --skip-repo; \
    groupadd --gid "${RUNNER_GID}" runner; \
    useradd --uid "${RUNNER_UID}" --gid runner --create-home --home-dir /home/runner \
        --shell /bin/bash runner; \
    usermod --append --groups sudo runner; \
    echo 'runner ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/runner; \
    chmod 0440 /etc/sudoers.d/runner

# The runner bundle is self-contained: bin/ holds the listener and its .NET
# runtime, externals/ the node builds that actions execute with.
#
# Every download lands in the shared /tmp/dl cache and only moves into place once
# curl finished, so a flaky link costs one retry instead of a full re-download and
# a truncated tarball can never be mistaken for a cached one.
RUN --mount=type=cache,target=/tmp/dl \
    set -eux; \
    tarball="actions-runner-linux-x64-${RUNNER_VERSION}.tar.gz"; \
    if [ ! -f "/tmp/dl/${tarball}" ]; then \
        curl -fsSL --retry 5 --retry-all-errors --retry-delay 3 \
            "${RUNNER_BASE_URL}/v${RUNNER_VERSION}/${tarball}" -o "/tmp/dl/${tarball}.part"; \
        mv "/tmp/dl/${tarball}.part" "/tmp/dl/${tarball}"; \
    fi; \
    tar -xzf "/tmp/dl/${tarball}" -C /home/runner; \
    chown -R runner:runner /home/runner; \
    su runner -c '/home/runner/bin/Runner.Listener --version'

# Node from the official tarball: the distribution package lags, and the actions
# a job downloads at runtime need a recent node.
#
# Node lives in its own prefix with no symlink farm in /usr/local/bin. Corepack
# `enable` writes its pnpm/yarn shims next to the corepack binary it is invoked
# through, and a job runs as `runner`, so that directory has to belong to the
# runner group: with the symlinks in root-owned /usr/local/bin, `corepack enable`
# fails the job with EACCES before any step runs.
RUN --mount=type=cache,target=/tmp/dl \
    set -eux; \
    mkdir -p /opt/node; \
    tarball="node-v${NODE_VERSION}-linux-x64.tar.xz"; \
    if [ ! -f "/tmp/dl/${tarball}" ]; then \
        curl -fsSL --retry 5 --retry-all-errors --retry-delay 3 \
            "${NODE_BASE_URL}/v${NODE_VERSION}/${tarball}" -o "/tmp/dl/${tarball}.part"; \
        mv "/tmp/dl/${tarball}.part" "/tmp/dl/${tarball}"; \
    fi; \
    tar -xJf "/tmp/dl/${tarball}" -C /opt/node --strip-components=1; \
    export PATH=/opt/node/bin:$PATH; \
    corepack enable; \
    chown -R root:runner /opt/node; \
    chmod -R g+w /opt/node/bin /opt/node/lib/node_modules; \
    chmod g+s /opt/node/bin; \
    node --version; npm --version; corepack --version

RUN --mount=type=cache,target=/tmp/dl \
    set -eux; \
    tarball="go${GO_VERSION}.linux-amd64.tar.gz"; \
    if [ ! -f "/tmp/dl/${tarball}" ]; then \
        curl -fsSL --retry 5 --retry-all-errors --retry-delay 3 \
            "${GO_BASE_URL}/${tarball}" -o "/tmp/dl/${tarball}.part"; \
        mv "/tmp/dl/${tarball}.part" "/tmp/dl/${tarball}"; \
    fi; \
    tar -xzf "/tmp/dl/${tarball}" -C /usr/local; \
    /usr/local/go/bin/go version

# Docker CLI, buildx and the daemon. The daemon is shipped although most pools
# never start it: a pool that sets `privileged: true` runs dockerd inside the
# runner container, because an ECI container group is a single container and a
# sidecar that never exits would keep the group alive after the job.
#
# containerd and ctr stay out on purpose — dockerd 29 uses the embedded
# containerd, exactly like the official docker:dind image — but the runc shim does
# not: the runtime spawns containerd-shim-runc-v2 for every container.
RUN --mount=type=cache,target=/tmp/dl \
    set -eux; \
    tarball="docker-${DOCKER_VERSION}.tgz"; \
    if [ ! -f "/tmp/dl/${tarball}" ]; then \
        curl -fsSL --retry 5 --retry-all-errors --retry-delay 3 \
            "${DOCKER_BASE_URL}/${tarball}" -o "/tmp/dl/${tarball}.part"; \
        mv "/tmp/dl/${tarball}.part" "/tmp/dl/${tarball}"; \
    fi; \
    tar -xzf "/tmp/dl/${tarball}" -C /usr/local/bin --strip-components=1 \
        docker/docker \
        docker/dockerd \
        docker/docker-init \
        docker/docker-proxy \
        docker/runc \
        docker/containerd-shim-runc-v2; \
    mkdir -p /usr/local/lib/docker/cli-plugins; \
    curl -fsSL --retry 5 --retry-all-errors --retry-delay 3 \
        "https://github.com/docker/buildx/releases/download/${BUILDX_VERSION}/buildx-${BUILDX_VERSION}.linux-amd64" \
        -o /usr/local/lib/docker/cli-plugins/docker-buildx; \
    chmod 0755 /usr/local/lib/docker/cli-plugins/docker-buildx; \
    install -d -o runner -g runner /home/runner/go; \
    git config --system --add safe.directory '*'; \
    docker --version; docker buildx version; dockerd --version

# ossutil is how the job hook reaches the image cache bucket. The alternative,
# mounting the bucket as a volume, needs a FlexVolume and an ossfs mount that our
# probes showed to be fragile; a plain HTTPS client with the instance role's STS
# token is one static binary and no mount at all.
RUN --mount=type=cache,target=/tmp/dl \
    set -eux; \
    archive="ossutil-${OSSUTIL_VERSION}-linux-amd64.zip"; \
    if [ ! -f "/tmp/dl/${archive}" ]; then \
        curl -fsSL --retry 5 --retry-all-errors --retry-delay 3 \
            "${OSSUTIL_BASE_URL}/${OSSUTIL_VERSION}/${archive}" -o "/tmp/dl/${archive}.part"; \
        mv "/tmp/dl/${archive}.part" "/tmp/dl/${archive}"; \
    fi; \
    unzip -p "/tmp/dl/${archive}" "ossutil-${OSSUTIL_VERSION}-linux-amd64/ossutil" \
        > /usr/local/bin/ossutil; \
    chmod 0755 /usr/local/bin/ossutil; \
    ossutil version

ENV LANG=C.UTF-8 \
    LC_ALL=C.UTF-8 \
    GOPATH=/home/runner/go \
    PATH=/opt/node/bin:/usr/local/go/bin:/home/runner/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    GIT_TERMINAL_PROMPT=0 \
    COREPACK_ENABLE_DOWNLOAD_PROMPT=0 \
    PIP_NO_CACHE_DIR=1 \
    PIP_BREAK_SYSTEM_PACKAGES=1 \
    NPM_CONFIG_FUND=false \
    NPM_CONFIG_AUDIT=false \
    ImageOS=ubuntu24 \
    RUNNER_MANUALLY_TRAP_SIG=1 \
    # Runs before each job, before the runner creates its containers. The script
    # is inert unless a privileged pool also names an image cache to restore.
    ACTIONS_RUNNER_HOOK_JOB_STARTED=/usr/local/bin/eci-job-started.sh \
    ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT=1

COPY --chmod=0755 entrypoint.sh /usr/local/bin/eci-entrypoint.sh
COPY --chmod=0755 runner.sh /usr/local/bin/eci-run.sh
COPY --chmod=0755 job-started.sh /usr/local/bin/eci-job-started.sh
COPY --chmod=0755 oss.sh /usr/local/bin/eci-oss
COPY --chmod=0644 eci-runner.conf /etc/supervisor/eci-runner.conf

USER runner
WORKDIR /home/runner
ENTRYPOINT ["/usr/local/bin/eci-entrypoint.sh"]

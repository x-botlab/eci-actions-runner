#!/bin/bash
# Runs before every job, ahead of the runner creating the job's containers.
#
# In a pool that restores an image cache this script owns the daemon: dockerd is
# not started at container boot (see dockerd.sh), so the store can be replaced
# here and the daemon started on top of it. That ordering is the whole point —
# a daemon already holding /var/lib/docker cannot survive the swap.
#
# It never fails a job. A missing or broken cache leaves an empty store and an
# ordinary pull, because a cache problem must not turn into a red build. There is
# also no timeout on hooks, so every external step is bounded here.
set -uo pipefail

log() { echo "eci-job-started: $*"; }

start_daemon() {
    # Detached from this shell on purpose: the hook exits as soon as it is done,
    # and the daemon has to outlive it until the runner ends the container.
    sudo -n setsid /usr/local/bin/dockerd \
        --host=unix:///var/run/docker.sock --group=runner --log-driver=local \
        < /dev/null > /tmp/dockerd.log 2>&1 &
    for _ in {1..90}; do
        docker info >/dev/null 2>&1 && break
        sleep 1
    done
    if docker info >/dev/null 2>&1; then
        log "dockerd is answering ($(docker images -q | wc -l) images in the store)"
    else
        log "dockerd is not answering, see /tmp/dockerd.log"
    fi
}

restore() {
    local bucket="$1" prefix="$2" content archive started
    # ossutil prints its elapsed time on stdout next to the payload, so the pointer
    # is parsed rather than taken verbatim.
    content=$(timeout 30 eci-oss cat "oss://$bucket/$prefix/latest" 2>/dev/null |
        grep -oE '\b[0-9a-f]{16}\b' | head -n1)
    if [[ -z "$content" ]]; then
        log "no cache entry at oss://$bucket/$prefix/latest"
        return 0
    fi
    archive="oss://$bucket/$prefix/$content.tar.zst"
    if ! timeout 30 eci-oss stat "$archive" >/dev/null 2>&1; then
        log "cache entry $content has no archive at $archive"
        return 0
    fi

    # A file rather than a pipe: ossutil writes its timing footer to the same
    # stdout as the payload, which zstd reads as a corrupt frame.
    if ! timeout 240 eci-oss cp "$archive" /tmp/eci-store.tar.zst >/dev/null 2>&1; then
        log "download of $archive failed, keeping the empty store"
        return 0
    fi

    started=$SECONDS
    # The glob has to be expanded by a root shell: /var/lib/docker is 0700 root, so
    # this user's shell cannot even list it, and `sudo rm -rf /var/lib/docker/*`
    # would pass the unexpanded pattern straight to rm.
    sudo -n sh -c 'rm -rf /var/lib/docker/* /var/lib/docker/.[!.]* 2>/dev/null || true'
    if timeout 240 bash -c "set -o pipefail; zstd -dc /tmp/eci-store.tar.zst | sudo -n tar --xattrs --xattrs-include='*' --acls --numeric-owner -C /var/lib/docker -xf -"; then
        log "restored image cache $content in $((SECONDS - started))s ($(sudo -n du -sh /var/lib/docker 2>/dev/null | cut -f1))"
    else
        log "restore of $content failed after $((SECONDS - started))s, keeping the empty store"
        sudo -n sh -c 'rm -rf /var/lib/docker/* /var/lib/docker/.[!.]* 2>/dev/null || true'
    fi
    rm -f /tmp/eci-store.tar.zst
}

log "workflow=${GITHUB_WORKFLOW:-} job=${GITHUB_JOB:-} event=${GITHUB_EVENT_NAME:-}"

if [[ "${ECI_RUNNER_PRIVILEGED:-}" != "1" ]]; then
    log "not a privileged pool, nothing to do"
    exit 0
fi

if docker info >/dev/null 2>&1; then
    # A daemon is already up, so the store belongs to it. Touching it now would
    # corrupt the daemon's containerd database, so this job runs on what it has.
    log "dockerd is already answering, skipping the restore"
    exit 0
fi

key="${ECI_RUNNER_IMAGE_CACHE:-}"
if [[ -z "$key" ]]; then
    # Privileged pool without a cache: the daemon normally runs from boot, so
    # getting here means it died during start-up. Leave it to the job rather than
    # guessing, but do start it.
    log "no image cache configured for this pool, starting the daemon"
    start_daemon
    exit 0
fi

bucket="${ECI_OSS_BUCKET:-}"
if [[ -n "$bucket" && -n "${ECI_OSS_ROLE:-}" && -n "${ECI_OSS_ENDPOINT:-}" ]]; then
    restore "$bucket" "eci-store/$key"
else
    log "image cache $key is configured without an OSS bucket, starting an empty store"
fi

start_daemon
exit 0

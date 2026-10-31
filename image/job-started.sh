#!/bin/bash
# Runs before every job, ahead of the runner creating the job's containers.
#
# This is the only place that can hand the daemon a warm image store: it runs as
# the runner user inside the runner container, so it can stop and start dockerd,
# and it is told which job is coming (GITHUB_WORKFLOW, GITHUB_JOB), which is what
# a cache key needs.
#
# It never fails a job. A missing or broken cache leaves an empty store and an
# ordinary pull, because a cache problem must not turn into a red build. There is
# also no timeout on hooks, so every external step is bounded here.
set -uo pipefail

log() { echo "eci-job-started: $*"; }

log "workflow=${GITHUB_WORKFLOW:-} job=${GITHUB_JOB:-} event=${GITHUB_EVENT_NAME:-}"

if [[ "${ECI_RUNNER_PRIVILEGED:-}" != "1" ]]; then
    log "not a privileged pool, nothing to do"
    exit 0
fi

key="${ECI_RUNNER_IMAGE_CACHE:-}"
if [[ -z "$key" ]]; then
    log "no image cache configured for this pool"
    exit 0
fi

bucket="${ECI_OSS_BUCKET:-}"
if [[ -z "$bucket" || -z "${ECI_OSS_ROLE:-}" || -z "${ECI_OSS_ENDPOINT:-}" ]]; then
    log "image cache $key is configured without an OSS bucket, skipping"
    exit 0
fi

prefix="eci-store/$key"
# ossutil prints its elapsed time on stdout next to the payload, so the pointer is
# parsed rather than taken verbatim.
latest="oss://$bucket/$prefix/latest"
content=$(timeout 30 eci-oss cat "$latest" 2>/dev/null | grep -oE '\b[0-9a-f]{16}\b' | head -n1)
if [[ -z "$content" ]]; then
    log "no cache entry at $latest"
    exit 0
fi
archive="oss://$bucket/$prefix/$content.tar.zst"
if ! timeout 30 eci-oss stat "$archive" >/dev/null 2>&1; then
    log "cache entry $content has no archive at $archive"
    exit 0
fi

# The store has to be replaced while no daemon is running: its metadata database
# only stays consistent with the snapshot directories if nothing is writing, and a
# daemon that still holds the old store cannot be restarted over a new one.
sudo -n pkill -TERM -x dockerd 2>/dev/null || true
for _ in {1..30}; do
    sudo -n pgrep -x dockerd >/dev/null 2>&1 || break
    sleep 1
done
if sudo -n pgrep -x dockerd >/dev/null 2>&1; then
    # Restarting is not an option here: the old daemon still owns the store, and
    # replacing it under a live daemon corrupts it.
    log "dockerd is still running, skipping the restore"
    exit 0
fi

started=$SECONDS
# The archive is downloaded to a file rather than streamed: ossutil writes its
# timing footer to the same stdout as the payload, which zstd reads as garbage.
# A file also lets zstd seek and verify the frame before anything is unpacked.
if ! timeout 240 eci-oss cp "$archive" /tmp/eci-store.tar.zst >/dev/null 2>&1; then
    log "download of $archive failed, continuing with an empty store"
else
    # The glob has to be expanded by a root shell: /var/lib/docker is 0700 root, so
    # this user's shell cannot even list it, and `sudo rm -rf /var/lib/docker/*`
    # would pass the unexpanded pattern straight to rm.
    sudo -n sh -c 'rm -rf /var/lib/docker/* /var/lib/docker/.[!.]* /var/run/docker.pid 2>/dev/null || true'
    if timeout 240 bash -c "set -o pipefail; zstd -dc /tmp/eci-store.tar.zst | sudo -n tar --xattrs --xattrs-include='*' --acls --numeric-owner -C /var/lib/docker -xf -"; then
        log "restored image cache $key/$content in $((SECONDS - started))s ($(sudo -n du -sh /var/lib/docker 2>/dev/null | cut -f1))"
    else
        log "restore of $key/$content failed after $((SECONDS - started))s, continuing with an empty store"
        sudo -n sh -c 'rm -rf /var/lib/docker/* /var/lib/docker/.[!.]* 2>/dev/null || true'
    fi
    rm -f /tmp/eci-store.tar.zst
fi

sudo -n /usr/local/bin/dockerd --host=unix:///var/run/docker.sock --group=runner --log-driver=local &
for _ in {1..60}; do
    docker info >/dev/null 2>&1 && break
    sleep 1
done
docker info >/dev/null 2>&1 || log "dockerd is not answering after the restore"
exit 0

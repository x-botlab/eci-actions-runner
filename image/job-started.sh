#!/bin/bash
# Runs before every job, ahead of the runner creating the job's containers.
#
# This is the only place that can hand the daemon a warm image store: it runs as
# the runner user inside the runner container, so it can stop and start dockerd,
# and it is told which job is coming (GITHUB_WORKFLOW, GITHUB_JOB), which is what
# a cache key needs.
#
# The archive is streamed straight from OSS with `eci-oss cat`, so a restore needs
# no scratch space: it goes bucket -> ossutil -> zstd -> tar -> /var/lib/docker.
#
# It never fails a job. A missing or broken cache falls back to an empty store
# and an ordinary pull, because a cache problem must not turn into a red build.
# There is also no timeout on hooks, so every external step is bounded here.
set -uo pipefail

log() { echo "eci-job-started: $*"; }

log "workflow=${GITHUB_WORKFLOW:-} job=${GITHUB_JOB:-} event=${GITHUB_EVENT_NAME:-}"

if [[ "${ECI_RUNNER_PRIVILEGED:-}" != "1" ]]; then
    log "not a privileged pool, nothing to do"
    exit 0
fi

if docker info >/dev/null 2>&1; then
    log "dockerd is already answering"
else
    log "dockerd is not answering yet"
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
content=$(timeout 30 eci-oss cat "oss://$bucket/$prefix/latest" 2>/dev/null | tr -d '[:space:]')
if [[ -z "$content" ]]; then
    log "no cache entry at oss://$bucket/$prefix/latest"
    exit 0
fi
archive="oss://$bucket/$prefix/$content.tar.zst"
if ! timeout 30 eci-oss stat "$archive" >/dev/null 2>&1; then
    log "cache entry $content has no archive at $archive"
    exit 0
fi

# The store has to be replaced while no daemon is running: its metadata database
# only stays consistent with the snapshot directories if nothing is writing.
sudo -n pkill -TERM -x dockerd 2>/dev/null || true
for _ in {1..20}; do
    docker info >/dev/null 2>&1 || break
    sleep 1
done

# The glob has to be expanded by a root shell: /var/lib/docker is 0700 root, so
# this user's shell cannot even list it, and `sudo rm -rf /var/lib/docker/*`
# would pass the unexpanded pattern straight to rm.
sudo -n sh -c 'rm -rf /var/lib/docker/* /var/lib/docker/.[!.]* 2>/dev/null || true'
if timeout 300 bash -c "set -o pipefail; eci-oss cat '$archive' | zstd -dc | sudo -n tar --xattrs --xattrs-include='*' --acls --numeric-owner -C /var/lib/docker -xf -"; then
    log "restored image cache $key/$content ($(sudo -n du -sh /var/lib/docker 2>/dev/null | cut -f1))"
else
    log "restore of $key/$content failed, continuing with an empty store"
    sudo -n sh -c 'rm -rf /var/lib/docker/* /var/lib/docker/.[!.]* 2>/dev/null || true'
fi

sudo -n /usr/local/bin/dockerd --host=unix:///var/run/docker.sock --group=runner --log-driver=local &
for _ in {1..60}; do
    docker info >/dev/null 2>&1 && break
    sleep 1
done
docker info >/dev/null 2>&1 || log "dockerd is not answering after the restore"
exit 0

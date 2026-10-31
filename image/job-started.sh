#!/bin/bash
# Runs before every job, ahead of the runner creating the job's containers.
#
# A privileged pool is the only place dockerd can run: an ECI container group is a
# single container and ECI offers no docker socket to mount. Starting it here
# instead of at container boot means a runner that never receives a job never
# starts a daemon, and the runner stays PID 1 so its exit code still reaches the
# scheduler.
#
# It never fails a job: if the daemon does not come up, the job's own docker
# commands fail with the real error instead of a hook that hides it.
set -uo pipefail

log() { echo "eci-job-started: $*"; }

log "workflow=${GITHUB_WORKFLOW:-} job=${GITHUB_JOB:-} event=${GITHUB_EVENT_NAME:-}"

if [[ "${ECI_RUNNER_PRIVILEGED:-}" != "1" ]]; then
    log "not a privileged pool, no daemon to start"
    exit 0
fi

if docker info >/dev/null 2>&1; then
    log "dockerd is already answering"
    exit 0
fi

# Detached on purpose: the hook exits as soon as it is done, and the daemon has to
# outlive it until the container ends with the job.
sudo -n setsid /usr/local/bin/dockerd \
    --host=unix:///var/run/docker.sock --group=runner --log-driver=local \
    < /dev/null > /tmp/dockerd.log 2>&1 &

for _ in {1..60}; do
    docker info >/dev/null 2>&1 && break
    sleep 1
done

if docker info >/dev/null 2>&1; then
    log "dockerd is answering"
else
    log "dockerd is not answering, see /tmp/dockerd.log"
fi
exit 0

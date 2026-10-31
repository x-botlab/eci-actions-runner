#!/bin/bash
# The supervisord program that owns dockerd.
#
# dockerd has to run as root while the container runs as the runner user, and its
# socket is created with --group=runner so job steps can use it without sudo.
#
# A pool that restores an image cache must not have a daemon yet when the job
# hook runs: the hook replaces /var/lib/docker, and a daemon holding the old store
# cannot survive that. In that case this program exits 0 straight away, supervisord
# records the program as stopped (autorestart is off), and the hook starts the
# daemon itself once the store is in place.
set -uo pipefail

if [[ -n "${ECI_RUNNER_IMAGE_CACHE:-}" ]]; then
    echo "eci-dockerd: image cache ${ECI_RUNNER_IMAGE_CACHE} configured, the job hook starts the daemon"
    exit 0
fi

exec /usr/bin/sudo --non-interactive /usr/local/bin/dockerd \
    --host=unix:///var/run/docker.sock --group=runner --log-driver=local

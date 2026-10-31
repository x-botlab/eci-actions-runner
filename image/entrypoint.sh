#!/bin/bash
# Entry point for the runner image.
#
# A privileged container exists for one reason here: dockerd has nowhere else to
# run, because an ECI container group is one container and there is no docker
# socket to mount. supervisord then owns two processes, dockerd and the runner,
# so the container still ends when the job does — see runner.sh.
#
# Everything else uses the plain path: one process, the runner.
set -euo pipefail

if [[ "${ECI_RUNNER_PRIVILEGED:-}" == "1" ]]; then
    exec supervisord --configuration /etc/supervisor/eci-runner.conf
fi

exec /usr/local/bin/eci-run.sh "$@"

#!/bin/bash
# The runner process, plus the docker daemon it may need.
#
# Under supervisord this is one of two programs; in a pool that is not privileged
# it is the only process in the container. Either way the container ends when the
# runner does, because an ECI container group only becomes terminal once every
# container has exited, and a terminal group is what the scheduler deletes
# immediately instead of waiting for the TTL.
#
# The JIT config arrives as ACTIONS_RUNNER_INPUT_JITCONFIG and is deliberately
# NOT passed as an argument: the runner reads that variable itself and removes it
# from its environment before starting a job, so neither `ps` nor the job steps
# can see the registration credential.
set -euo pipefail

# run.sh resolves its siblings relative to itself, so the working directory has
# to be the runner root.
cd /home/runner

# Under supervisord the container outlives this script: supervisord is PID 1 and
# keeps dockerd running. Every exit path, including the failures below, therefore
# has to stop it, or a job that never started would hold the container group
# until its TTL.
if [[ -n "${SUPERVISOR_ENABLED:-}" ]]; then
    trap 'supervisorctl --configuration /etc/supervisor/eci-runner.conf shutdown >/dev/null 2>&1 || true' EXIT
fi

if [[ "${ECI_RUNNER_PRIVILEGED:-}" == "1" ]]; then
    # dockerd is a sibling under supervisord and takes a moment to answer. A job
    # that starts before the socket is up fails its first docker command, so wait
    # here; a daemon that never comes up fails the container instead of leaving a
    # runner behind that cannot do its job.
    for _ in {1..60}; do
        docker info >/dev/null 2>&1 && break
        sleep 1
    done
    if ! docker info >/dev/null 2>&1; then
        echo "eci-runner: dockerd is not answering on the default socket after 60s" >&2
        exit 70
    fi
fi

if [[ $# -eq 0 && -z "${ACTIONS_RUNNER_INPUT_JITCONFIG:-}" ]]; then
    echo "eci-runner: no JIT config in ACTIONS_RUNNER_INPUT_JITCONFIG and no run.sh arguments given" >&2
    exit 64
fi

# Without a supervisor there is nothing to shut down, and exec keeps run.sh's
# signal handling intact.
if [[ -z "${SUPERVISOR_ENABLED:-}" ]]; then
    exec ./run.sh "$@"
fi

# No exec here: the EXIT trap above is what stops supervisord, and set -e makes
# this script exit with the runner's status.
./run.sh "$@"

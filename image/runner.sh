#!/bin/bash
# The container's only process: it is PID 1, so the container ends when the runner
# does. An ECI container group only becomes terminal once every container in it has
# exited, and a terminal group is what the scheduler deletes immediately instead of
# waiting for the TTL.
#
# A privileged pool gets its docker daemon from the job hook (job-started.sh), not
# from here: the daemon only has to exist once a job is assigned, and keeping it out
# of this process tree means nothing sits between the runner and the exit code.
#
# The JIT config arrives as ACTIONS_RUNNER_INPUT_JITCONFIG and is deliberately NOT
# passed as an argument: the runner reads that variable itself and removes it from
# its environment before starting a job, so neither `ps` nor the job steps can see
# the registration credential.
set -euo pipefail

# run.sh resolves its siblings relative to itself, so the working directory has to
# be the runner root.
cd /home/runner

if [[ $# -eq 0 && -z "${ACTIONS_RUNNER_INPUT_JITCONFIG:-}" ]]; then
    echo "eci-runner: no JIT config in ACTIONS_RUNNER_INPUT_JITCONFIG and no run.sh arguments given" >&2
    exit 64
fi

exec ./run.sh "$@"

#!/bin/bash
# Entry point for the runner image.
#
# The JIT config arrives as ACTIONS_RUNNER_INPUT_JITCONFIG and is deliberately
# NOT passed as an argument: the runner reads that variable itself and removes it
# from its environment before starting a job, so neither `ps` nor the job steps
# can see the registration credential.
set -euo pipefail

# run.sh resolves its siblings relative to itself, so the working directory has
# to be the runner root.
cd /home/runner

if [[ $# -gt 0 ]]; then
    exec ./run.sh "$@"
fi

if [[ -z "${ACTIONS_RUNNER_INPUT_JITCONFIG:-}" ]]; then
    echo "eci-runner: no JIT config in ACTIONS_RUNNER_INPUT_JITCONFIG and no run.sh arguments given" >&2
    exit 64
fi

exec ./run.sh

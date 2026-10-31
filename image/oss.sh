#!/bin/bash
# ossutil with the container group's instance role as credentials.
#
# ECI hands the role's STS token to the container through the same metadata
# endpoint ECS uses, so nothing secret has to travel in the container spec: the
# controller only names the role. The token outlives a job, and every call
# fetches its own, which keeps a long job from running into an expiry.
#
# Usage: eci-oss <ossutil arguments>, for example
#        eci-oss ls oss://bucket/prefix/
#        eci-oss cat oss://bucket/key
set -uo pipefail

role="${ECI_OSS_ROLE:-}"
endpoint="${ECI_OSS_ENDPOINT:-}"
if [[ -z "$role" || -z "$endpoint" ]]; then
    echo "eci-oss: ECI_OSS_ROLE and ECI_OSS_ENDPOINT must be set" >&2
    exit 2
fi

# STS credentials force signature v4, which needs the bucket's region. The
# controller passes it; deriving it from the endpoint keeps manual runs working.
region="${ECI_OSS_REGION:-}"
if [[ -z "$region" ]]; then
    region=$(sed -E 's/^oss-//; s/\.aliyuncs\.com$//; s/-internal$//' <<<"$endpoint")
fi

metadata="http://100.100.100.200/latest/meta-data/ram/security-credentials/$role"
creds=$(curl -fsS -m 5 "$metadata") || {
    echo "eci-oss: no credentials for role $role" >&2
    exit 3
}
ak=$(jq -r '.AccessKeyId // empty' <<<"$creds")
sk=$(jq -r '.AccessKeySecret // empty' <<<"$creds")
token=$(jq -r '.SecurityToken // empty' <<<"$creds")
if [[ -z "$ak" || -z "$sk" || -z "$token" ]]; then
    echo "eci-oss: role $role returned no credentials" >&2
    exit 3
fi

exec ossutil \
    --access-key-id "$ak" \
    --access-key-secret "$sk" \
    --sts-token "$token" \
    --endpoint "$endpoint" \
    --region "$region" \
    "$@"

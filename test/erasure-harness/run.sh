#!/usr/bin/env bash
# Local full erasure harness (account-erasure ticket 10): one command builds what is missing,
# brings the stack up, seeds it and runs the scenarios, each printing PASS or FAIL.
#
#   test/erasure-harness/run.sh                 # everything, default scenario order
#   test/erasure-harness/run.sh 1 4 5           # up + seed + only these scenarios
#   test/erasure-harness/run.sh --driver scenario 6   # a driver command against a running stack
#   test/erasure-harness/down.sh                # removes containers, volumes, network and .state
#
# Needs Docker and git; nothing else on the host (the driver container carries bash 5, curl,
# jq, psql and redis-cli). Without registry credentials, point DOCKER_CONFIG at a directory
# holding an empty config.json. Environment: SKYLAB_REPOS (sibling repositories, see build.sh),
# HARNESS_PROJECT (compose project, default skylab-erasure-harness), HARNESS_ACCOUNT_CENTER=0
# (skip Account Center). Runs with macOS bash 3.2.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
STATE=$HERE/.state
PROJECT=${HARNESS_PROJECT:-skylab-erasure-harness}
export HARNESS_PROJECT=$PROJECT

"$HERE/build.sh"
# shellcheck source=/dev/null
. "$STATE/images.env"

mkdir -p "$STATE"
docker run --rm -v "$HERE:$HERE" -w "$HERE" -e HARNESS_STATE_DIR="$STATE" \
  --entrypoint /bin/bash "$HARNESS_IMAGE_DRIVER" lib/init-state.sh

compose() {
  docker compose -p "$PROJECT" -f "$HERE/compose.yaml" \
    --env-file "$STATE/compose.env" --env-file "$STATE/images.env" "$@"
}

if [ "${1:-}" = "--driver" ]; then
  shift
  compose --profile driver run --rm --no-deps -T \
    -e HARNESS_ACCOUNT_CENTER="${HARNESS_ACCOUNT_CENTER:-1}" driver lib/main.sh "$@"
  exit $?
fi
compose --profile driver run --rm --no-deps -T \
  -e HARNESS_ACCOUNT_CENTER="${HARNESS_ACCOUNT_CENTER:-1}" driver lib/main.sh all "$@"

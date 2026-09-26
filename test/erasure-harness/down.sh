#!/usr/bin/env bash
# Tears the erasure harness down: its containers, volumes and network, and the throwaway state
# (.state/: certificates, passwords, evidence). Images are kept; HARNESS_REMOVE_IMAGES=1 removes
# the ones build.sh made. HARNESS_KEEP_EVIDENCE=1 keeps .state/evidence and results.tsv.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
STATE=$HERE/.state
PROJECT=${HARNESS_PROJECT:-skylab-erasure-harness}

if [ -f "$STATE/compose.env" ] && [ -f "$STATE/images.env" ]; then
  docker compose -p "$PROJECT" -f "$HERE/compose.yaml" --profile driver --profile tools --profile account-center \
    --env-file "$STATE/compose.env" --env-file "$STATE/images.env" down --volumes --remove-orphans --timeout 5 || true
fi
# Belt and braces: anything still labelled with the project.
ids=$(docker ps -aq --filter "label=com.docker.compose.project=$PROJECT")
[ -z "$ids" ] || docker rm -f $ids >/dev/null
vols=$(docker volume ls -q --filter "label=com.docker.compose.project=$PROJECT")
[ -z "$vols" ] || docker volume rm $vols >/dev/null
nets=$(docker network ls -q --filter "label=com.docker.compose.project=$PROJECT")
[ -z "$nets" ] || docker network rm $nets >/dev/null

if [ "${HARNESS_REMOVE_IMAGES:-0}" = 1 ] && [ -f "$STATE/images.env" ]; then
  for image in $(cut -d= -f2 "$STATE/images.env"); do docker image rm "$image" >/dev/null 2>&1 || true; done
fi
if [ "${HARNESS_KEEP_EVIDENCE:-0}" = 1 ]; then
  find "$STATE" -mindepth 1 -maxdepth 1 ! -name evidence ! -name results.tsv ! -name images.env ! -name images.lock -exec rm -rf {} +
else
  find "$STATE" -mindepth 1 -maxdepth 1 ! -name images.env ! -name images.lock ! -name build -exec rm -rf {} + 2>/dev/null || true
fi
echo "erasure harness $PROJECT removed"

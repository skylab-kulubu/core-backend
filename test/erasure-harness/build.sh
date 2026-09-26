#!/usr/bin/env bash
# Builds the candidate images of the local full erasure harness (account-erasure ticket 10)
# from origin/main of each repository and records what was built.
#
#   test/erasure-harness/build.sh               # every image, skipping the ones already built
#   test/erasure-harness/build.sh keycloak cms  # only these
#   HARNESS_REBUILD=1 test/erasure-harness/build.sh
#
# Repositories are read from SKYLAB_REPOS (default: the directory that holds this core-backend
# checkout's main repository, where e-skylab-keycloak, account-center, skymail-backend and
# cms-backend sit next to it). Each tree comes from `git archive origin/main`, never from a
# working copy, so a dirty checkout cannot leak into an image. HARNESS_FETCH=0 skips the fetch.
#
# Output in .state/: images.env (the tags compose reads) and images.lock (repository, commit,
# image id and the pinned base images of every image).
#
# Runs with macOS bash 3.2. Docker needs no registry credentials here: point DOCKER_CONFIG at a
# directory with an empty config.json when the default credential helper hangs.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
STATE=${HARNESS_STATE_DIR:-$HERE/.state}
if [ -z "${SKYLAB_REPOS:-}" ]; then
  common=$(git -C "$HERE" rev-parse --path-format=absolute --git-common-dir)
  SKYLAB_REPOS=$(cd "$common/../.." && pwd)
fi
FETCH=${HARNESS_FETCH:-1}
REBUILD=${HARNESS_REBUILD:-0}
PREFIX=skylab-erasure-harness

ALL_TARGETS="core keycloak account-center skymail cms stubs driver"
TARGETS=${*:-$ALL_TARGETS}

mkdir -p "$STATE/build"
touch "$STATE/images.env" "$STATE/images.lock"

log() { printf '[build] %s\n' "$*"; }

repo_of() {
  case $1 in
    core) echo core-backend ;;
    keycloak) echo e-skylab-keycloak ;;
    account-center) echo account-center ;;
    skymail) echo skymail-backend ;;
    cms) echo cms-backend ;;
    *) echo "" ;;
  esac
}

env_name() {
  printf 'HARNESS_IMAGE_%s' "$(printf '%s' "$1" | tr 'a-z-' 'A-Z_')"
}

# record NAME TAG REPO SHA CONTEXT: writes images.env and images.lock entries for one image.
record() {
  local name=$1 tag=$2 repo=$3 sha=$4 context=$5 key id bases tmp
  # Parallel builds share the two files: one writer at a time.
  until mkdir "$STATE/.record.lock" 2>/dev/null; do sleep 1; done
  trap 'rmdir "$STATE/.record.lock" 2>/dev/null || true' RETURN
  key=$(env_name "$name")
  id=$(docker image inspect --format '{{.Id}}' "$tag")
  bases=$(awk '
      $1 == "FROM" { for (i = 2; i <= NF; i++) if ($i !~ /^--/) { if (!($i in stage)) print $i; break }
                     if (toupper($(NF - 1)) == "AS") stage[$NF] = 1 }
      $1 == "ARG" && $2 ~ /IMAGE=/ { sub(/^[^=]*=/, "", $2); print $2 }' "$context"/Dockerfile 2>/dev/null \
    | grep -v '^\$' | sort -u | paste -sd' ' - || true)
  tmp=$(mktemp)
  grep -v "^$key=" "$STATE/images.env" >"$tmp" || true
  printf '%s=%s\n' "$key" "$tag" >>"$tmp"
  mv "$tmp" "$STATE/images.env"
  tmp=$(mktemp)
  grep -v "^$name " "$STATE/images.lock" >"$tmp" || true
  printf '%s repo=%s commit=%s image=%s id=%s bases=[%s] built=%s\n' \
    "$name" "$repo" "$sha" "$tag" "$id" "$bases" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >>"$tmp"
  sort "$tmp" >"$STATE/images.lock"
  rm -f "$tmp"
}

build_repo_image() {
  local name=$1 repo dir sha short tag context
  repo=$(repo_of "$name")
  dir="$SKYLAB_REPOS/$repo"
  [ -d "$dir/.git" ] || [ -f "$dir/.git" ] || { log "missing repository $dir"; exit 1; }
  if [ "$FETCH" = 1 ]; then
    git -C "$dir" fetch --quiet origin main
  fi
  sha=$(git -C "$dir" rev-parse origin/main)
  short=$(printf '%s' "$sha" | cut -c1-12)
  tag="$PREFIX/$name:$short"
  context="$STATE/build/$name-$short"
  if [ "$REBUILD" != 1 ] && docker image inspect "$tag" >/dev/null 2>&1; then
    log "$name: $tag already built"
  else
    rm -rf "$context"
    mkdir -p "$context"
    git -C "$dir" archive origin/main | tar -x -C "$context"
    log "$name: building $tag from $repo origin/main $sha"
    docker build --progress=plain -t "$tag" "$context"
  fi
  [ -d "$context" ] || { mkdir -p "$context"; git -C "$dir" archive origin/main | tar -x -C "$context"; }
  record "$name" "$tag" "$repo" "$sha" "$context"
}

build_local_image() {
  local name=$1 context=$2 hash tag
  hash=$(cd "$context" && find . -type f ! -name '*.md' | LC_ALL=C sort | xargs shasum -a 256 | shasum -a 256 | cut -c1-12)
  tag="$PREFIX/$name:$hash"
  if [ "$REBUILD" != 1 ] && docker image inspect "$tag" >/dev/null 2>&1; then
    log "$name: $tag already built"
  else
    log "$name: building $tag from $context"
    docker build --progress=plain -t "$tag" "$context"
  fi
  record "$name" "$tag" "core-backend(test/erasure-harness)" "tree-$hash" "$context"
}

for target in $TARGETS; do
  case $target in
    core | keycloak | account-center | skymail | cms) build_repo_image "$target" ;;
    stubs) build_local_image stubs "$HERE/stubs" ;;
    driver) build_local_image driver "$HERE/driver" ;;
    *) log "unknown target $target (known: $ALL_TARGETS)"; exit 2 ;;
  esac
done
log "images: $STATE/images.env"

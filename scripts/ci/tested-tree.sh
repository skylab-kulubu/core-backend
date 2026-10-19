#!/usr/bin/env bash
# tested-tree.sh TREE: exits 0 when a fully green full run of the Checks
# workflow (.github/workflows/checks.yml) has tested exactly the git tree
# TREE, 1 when none has (yet).
#
# production is a squash of main, so the tree is compared, not the commit.
# Only push, schedule and workflow_dispatch runs count: a pull_request run
# tests GitHub's merge commit, not its head. Full and fully green: its job
# "scope (full)" and every other job passed (a skipped job, as in a run that
# promoted an already tested tree, does not count), the timings job aside,
# which only records how long the tests took.
#
# Needs GH_TOKEN (actions: read) and GITHUB_REPOSITORY; reads 21 days back.
set -euo pipefail

tree=${1:?usage: tested-tree.sh TREE}
since=$(date -u -d '21 days ago' +%Y-%m-%d)
runs=$(gh api --paginate -X GET "repos/$GITHUB_REPOSITORY/actions/workflows/checks.yml/runs" \
  -f status=success -f "created=>=$since" -f per_page=100 \
  --jq '.workflow_runs[] | select(.event == "push" or .event == "schedule" or .event == "workflow_dispatch") | "\(.id) \(.head_sha)"')
declare -A tree_of=()
while read -r run_id head_sha; do
  [[ $run_id =~ ^[0-9]+$ && $head_sha =~ ^[0-9a-f]{40}$ ]] || continue
  if [[ -z ${tree_of[$head_sha]+set} ]]; then
    tree_of[$head_sha]=$(gh api "repos/$GITHUB_REPOSITORY/git/commits/$head_sha" --jq .tree.sha)
  fi
  [[ ${tree_of[$head_sha]} == "$tree" ]] || continue
  jobs=$(gh api --paginate "repos/$GITHUB_REPOSITORY/actions/runs/$run_id/jobs?per_page=100" \
    --jq '.jobs[] | select(.name != "timings") | "\(.conclusion) \(.name)"')
  if grep -qxF 'success scope (full)' <<<"$jobs" && grep -q '^success test (' <<<"$jobs" && ! grep -qv '^success ' <<<"$jobs"; then
    echo "Checks run $run_id ($head_sha) tested tree $tree in full; every job passed."
    exit 0
  fi
done <<<"$runs"
echo "No fully green full Checks run of tree $tree since $since."
exit 1

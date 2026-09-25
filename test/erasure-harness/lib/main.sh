#!/usr/bin/env bash
# Driver entry point (inside the driver container). run.sh calls it; it can also be called again
# against a running stack:
#   main.sh up                  bring the stack up and provision it
#   main.sh seed                seed every person's data in every service
#   main.sh scenario N [N...]   run scenarios (1..11)
#   main.sh all [N...]          up, seed, scenarios (default order), summary
#   main.sh summary             print the PASS/FAIL table
set -Eeuo pipefail

source "$(dirname -- "${BASH_SOURCE[0]}")/common.sh"
source "$HARNESS_DIR/lib/provision.sh"
source "$HARNESS_DIR/lib/seed.sh"
source "$HARNESS_DIR/lib/account-center.sh"
shopt -s nullglob
for file in "$HARNESS_DIR"/lib/scenarios/*.sh; do
  # shellcheck source=/dev/null
  source "$file"
done

DEFAULT_ORDER=(1 4 5 6 7 9 2 3 8 11 10)

run_scenario() {
  local n=$1
  if ! declare -F "scenario_$n" >/dev/null; then
    printf '%s\tNOT-DONE\t%s\tno scenario implementation\n' "S$n" "$(date -u +%FT%TZ)" >>"$RESULTS"
    printf '\n==> SCENARIO S%s: NOT-DONE\n\n' "$n"
    return 0
  fi
  # A failing scenario must not stop the others: its checks are recorded, and an unexpected
  # error inside it is recorded as a failure of that scenario.
  if ! ( set -Eeuo pipefail; "scenario_$n" ); then
    if ! grep -q "^S$n	\(PASS\|FAIL\)" "$RESULTS" 2>/dev/null; then
      printf '%s\tFAIL\t%s\taborted\n' "S$n" "$(date -u +%FT%TZ)" >>"$RESULTS"
      printf '\n==> SCENARIO S%s: FAIL (aborted)\n\n' "$n"
    fi
  fi
}

summary() {
  printf '\n%-5s %-9s %s\n' SCEN STATUS DETAIL
  awk -F'\t' '$2 == "PASS" || $2 == "FAIL" || $2 == "NOT-DONE" { last[$1] = $2 "\t" $4; order[$1] = NR }
    END { for (s in last) print order[s] "\t" s "\t" last[s] }' "$RESULTS" | sort -n | cut -f2- \
    | while IFS=$'\t' read -r s status detail; do printf '%-5s %-9s %s\n' "$s" "$status" "$detail"; done
}

command=${1:-all}
shift || true
case $command in
  up)
    provision_infrastructure
    provision_keycloak
    provision_services
    [[ ${HARNESS_ACCOUNT_CENTER:-1} == 0 ]] || provision_account_center
    ;;
  seed) seed_all ;;
  scenario)
    for n in "$@"; do run_scenario "$n"; done
    summary
    ;;
  all)
    provision_infrastructure
    provision_keycloak
    provision_services
    [[ ${HARNESS_ACCOUNT_CENTER:-1} == 0 ]] || provision_account_center
    seed_all
    order=("$@")
    ((${#order[@]})) || order=("${DEFAULT_ORDER[@]}")
    for n in "${order[@]}"; do run_scenario "$n"; done
    summary
    ;;
  summary) summary ;;
  *)
    printf 'unknown command %s\n' "$command" >&2
    exit 2
    ;;
esac

#!/usr/bin/env bash
# Scenario 9 — logs (spec §2.7): every container log of the run is searched for the erased
# persons' addresses, names and subject ids. Addresses and names: 0 everywhere. A subject id
# may appear only in Keycloak's own event lines.

scenario_9() {
  local dir=$EVIDENCE/logs service p subject total hits line
  scenario_begin S9 'log scan: no address or name of an erased person in any container log; subject only in Keycloak events'
  mkdir -p "$dir"
  for service in $(dc --profile account-center ps -a --services 2>/dev/null); do
    dc --profile account-center logs --no-color --no-log-prefix --timestamps "$service" >"$dir/$service.log" 2>&1 || true
  done
  local erased=()
  for p in "${PERSONS[@]}"; do
    [[ $(pg super_skylab "SELECT status FROM account_deletion_requests WHERE subject_id = '${P_ID[$p]}'") == completed ]] && erased+=("$p")
  done
  note "erased persons scanned: ${erased[*]:-none}; services: $(ls "$dir" | sed 's/\.log$//' | paste -sd, -)"
  check 'at least one erased person to scan for' test "${#erased[@]}" -gt 0

  for p in "${erased[@]}"; do
    total=0
    for service in "$dir"/*.log; do
      hits=$(grep -ciF -f <(pii_needles "$p") "$service" || true)
      if ((hits > 0)); then
        log "        $p named $hits time(s) in $(basename "$service" .log)"
        grep -iF -f <(pii_needles "$p") "$service" | head -n 3 | cut -c1-200 | sed 's/^/          | /' >&2
      fi
      total=$((total + hits))
    done
    check "no address or name of $p in any log" eq "$total" 0

    subject=${P_ID[$p]}
    total=0
    for service in "$dir"/*.log; do
      [[ $(basename "$service") == keycloak.log ]] && continue
      hits=$(grep -ciF "$subject" "$service" || true)
      ((hits == 0)) || log "        subject of $p in $(basename "$service" .log): $hits"
      total=$((total + hits))
    done
    check "subject of $p appears in no service log but Keycloak's" eq "$total" 0
    hits=$(grep -iF "$subject" "$dir/keycloak.log" | grep -vc 'type="\?[A-Z_]*"\?,\? \|type=[A-Z_]' || true)
    check "subject of $p in Keycloak's log only on event lines ($hits other lines)" eq "$hits" 0
  done

  # Spec §2.7 asks error logs to carry request_id, step and code. core's worker error lines
  # carry step and code but not the request id.
  line=$(grep -m1 'account erasure worker: account erasure erase_' "$dir/core.log" || true)
  if [[ -n $line ]]; then
    [[ $line == *request_id* ]] || note "core's worker error lines carry no request_id (spec §2.7 asks for it): '${line#* }'"
  fi
  scenario_end
}

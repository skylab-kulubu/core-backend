#!/usr/bin/env bash
# Scenario 10 — Keycloak's admin and user events after the saga (ticket 09's evidence, taken in
# this harness with production's event settings: user events 30 days, admin event details on).
# It records which event carries which personal field for every erased person; it does not
# decide the remedy (ticket 09). PASS means the evidence was taken; the finding is a note.

scenario_10() {
  local p subject out=$EVIDENCE/s10-keycloak-events.tsv events with_pii=0 delete_rep total=0
  scenario_begin S10 "Keycloak admin/user events of erased persons: evidence for ticket 09"
  printf 'person\tevent\toperation_or_type\tresource\tpersonal fields present\n' >"$out"
  for p in "${PERSONS[@]}"; do
    subject=${P_ID[$p]}
    [[ $(pg super_skylab "SELECT status FROM account_deletion_requests WHERE subject_id = '$subject'") == completed ]] || continue
    kc GET "/admin-events?resourcePath=users/$subject*&max=200"
    events=$HTTP_BODY
    while IFS=$'\t' read -r op path fields; do
      printf '%s\tadmin\t%s\t%s\t%s\n' "$p" "$op" "${path//$subject/<sub>}" "${fields:--}" >>"$out"
      total=$((total + 1))
      [[ $fields == *mail* || $fields == *Name* ]] && with_pii=$((with_pii + 1))
      [[ $op == DELETE ]] && delete_rep=$fields
    done < <(jq -r '.[] | [.operationType, .resourcePath,
        ((.representation // "{}") | (try fromjson catch {}) as $r
         | [ (if ($r | type) != "object" then empty else
               ((if ($r.email // "") != "" then "email" else empty end),
                (if ($r.firstName // "") != "" then "firstName" else empty end),
                (if ($r.lastName // "") != "" then "lastName" else empty end),
                (if ($r.username // "") != "" then "username" else empty end),
                (if ($r.attributes.schoolEmail // []) != [] then "schoolEmail" else empty end),
                (if ($r.attributes.personalEmail // []) != [] then "personalEmail" else empty end)) end) ]
         | join(","))] | @tsv' <<<"$events")
    kc GET "/events?user=$subject&max=200"
    jq -r --arg p "$p" '.[] | [$p, "user", .type, (.clientId // ""), ((.details // {}) | keys | join(","))] | @tsv' <<<"$HTTP_BODY" >>"$out"
  done
  log "  $(wc -l <"$out") event rows written to $out"
  check 'admin events of the erased persons were read' test "$total" -gt 0
  note "Keycloak keeps $with_pii of $total admin events of erased persons with e-mail or name in the representation (which operation carries what: $out); DELETE carries: ${delete_rep:-nothing}. Remedy is ticket 09's decision."
  check 'DELETE events carry at most id and username' grep -Eqx '(username)?' <<<"${delete_rep:-}"
  scenario_end
}

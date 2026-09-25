#!/usr/bin/env bash
# Seeds every person's data in every service, the way the product would have written it:
# core's user row by JIT (the person's own token), then tickets, check-ins and certificates;
# SkyMail recipients, sends, queue rows, templates and a Mail onayı; CMS items, blocks and
# drafts; Forms forms, responses and collaborators. Each row that names the person is one the
# erasure must change; the bystander p8 gets the same rows and must keep them.
# Sourced by main.sh.

sql_quote() { printf "'%s'" "${1//\'/\'\'}"; }

# uuid_of TEXT: a stable UUID for a seed row.
uuid_of() { printf '%s' "$1" | md5sum | sed -E 's/^(.{8})(.{4})(.{4})(.{4})(.{12}).*/\1-\2-\3-\4-\5/'; }

seed_core_jit() {
  local person=$1 token
  token=$(user_token "$person" core)
  http GET "$CORE/v1/users/me" -H "Authorization: Bearer $token"
  [[ $HTTP_STATUS == 200 ]] || { log "core JIT for $person answered $HTTP_STATUS"; return 1; }
}

seed_core_shared() {
  pg super_skylab "
    INSERT INTO events (id, name, location, owner_team, active)
    VALUES ('$(uuid_of event)', 'Harness Etkinliği', 'Davutpaşa', 'SKY LAB', true) ON CONFLICT DO NOTHING;
    INSERT INTO event_days (id, event_id, name) VALUES ('$(uuid_of day)', '$(uuid_of event)', 'Gün 1') ON CONFLICT DO NOTHING;
    INSERT INTO sessions (id, event_day_id, title, session_type)
    VALUES ('$(uuid_of session)', '$(uuid_of day)', 'Açılış', 'talk') ON CONFLICT DO NOTHING;" >/dev/null
}

seed_core_person() {
  local p=$1 subject=${P_ID[$1]} name first last school personal
  name=$(sql_quote "$(full_name "$p")") first=$(sql_quote "${P_FIRST[$p]}") last=$(sql_quote "${P_LAST[$p]}")
  school=$(sql_quote "${P_SCHOOL[$p]}")
  # The guest rows carry the address in another case: the match must ignore it.
  personal=$(sql_quote "$(tr 'a-z' 'A-Z' <<<"${P_PERSONAL[$p]:0:1}")${P_PERSONAL[$p]:1}")
  pg super_skylab "
    UPDATE users SET phone = '+90 555 000 00 0${p#p}', school_email = $school WHERE id = '$subject';
    INSERT INTO tickets (id, event_id, ticket_type, owner_id)
    VALUES ('$(uuid_of "$p-ticket-owned")', '$(uuid_of event)', 'participant', '$subject');
    INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email, guest_phone_number)
    VALUES ('$(uuid_of "$p-ticket-guest")', '$(uuid_of event)', 'guest', $first, $last, $personal, '+90 555 111 11 1${p#p}');
    INSERT INTO ticket_checkins (id, ticket_id, event_day_id, session_id) VALUES
      ('$(uuid_of "$p-checkin-owned")', '$(uuid_of "$p-ticket-owned")', '$(uuid_of day)', '$(uuid_of session)'),
      ('$(uuid_of "$p-checkin-guest")', '$(uuid_of "$p-ticket-guest")', '$(uuid_of day)', '$(uuid_of session)');
    INSERT INTO certificates (id, event_id, ticket_id, owner_id, serial, recipient_name, recipient_email, event_name, owner_team,
                              verify_url, pdf, pdf_key, pdf_sha256)
    VALUES ('$(uuid_of "$p-cert-owned")', '$(uuid_of event)', '$(uuid_of "$p-ticket-owned")', '$subject', 'HX-${p^^}-OWNED',
            $name, $school, 'Harness Etkinliği', 'SKY LAB', 'https://skyl.app/c/HX-${p^^}-OWNED', '\\x25504446', 'certificates/$p-owned.pdf',
            encode(sha256('\\x25504446'::bytea), 'hex')),
           ('$(uuid_of "$p-cert-guest")', '$(uuid_of event)', '$(uuid_of "$p-ticket-guest")', NULL, 'HX-${p^^}-GUEST',
            $name, $personal, 'Harness Etkinliği', 'SKY LAB', 'https://skyl.app/c/HX-${p^^}-GUEST', '\\x25504446', 'certificates/$p-guest.pdf',
            encode(sha256('\\x25504446'::bytea), 'hex'));" >/dev/null
}

seed_skymail_shared() {
  pg skymail "
    INSERT INTO mailing_lists (id, name, description) VALUES ('$(uuid_of list)', 'Harness listesi', 'harness') ON CONFLICT DO NOTHING;" >/dev/null
}

seed_skymail_person() {
  local p=$1 subject=${P_ID[$1]} name school personal mixed bystander=${P_ID[p8]} bystander_mail=${P_SCHOOL[p8]} tpl ver
  name=$(sql_quote "$(full_name "$p")")
  school=$(sql_quote "${P_SCHOOL[$p]}") personal=$(sql_quote "${P_PERSONAL[$p]}")
  mixed=$(sql_quote "$(tr 'a-z' 'A-Z' <<<"${P_PERSONAL[$p]:0:1}")${P_PERSONAL[$p]:1}")
  tpl=$(uuid_of "$p-template") ver=$(uuid_of "$p-template-v1")
  pg skymail "
    INSERT INTO recipients (id, full_name, email) VALUES ('$(uuid_of "$p-recipient")', $name, $mixed);
    INSERT INTO mailing_list_recipients (mail_list_id, recipient_id) VALUES ('$(uuid_of list)', '$(uuid_of "$p-recipient")');
    -- A template the person authored (template_versions.author_sub) and archived.
    INSERT INTO templates (id, name, html_content, plain_text_content, react_email_content, subject, archived_at, archived_by)
    VALUES ('$tpl', 'Harness şablonu $p', '<p>Merhaba {{.ad}}</p>', 'Merhaba {{.ad}}', '', 'Duyuru', now(), '$subject');
    INSERT INTO template_versions (id, template_id, seq, subject, html_source, main_mode, html_content, plain_text_content,
                                   author_kind, author_sub, author_name, published_at, name)
    VALUES ('$ver', '$tpl', 1, 'Duyuru', '<p>Merhaba {{.ad}}</p>', 'html', '<p>Merhaba {{.ad}}</p>', 'Merhaba {{.ad}}',
            'operator', '$subject', $name, now(), 'Harness şablonu $p');
    UPDATE templates SET published_version_id = '$ver' WHERE id = '$tpl';
    -- A send by the person to the bystander, whose body names the sender.
    INSERT INTO mail_tasks (id, sent_by, template_id, body_variables)
    VALUES ('$(uuid_of "$p-task-by")', '$subject', '$tpl', jsonb_build_object('imza', $name));
    INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, body_html, status, attempts)
    VALUES ('$(uuid_of "$p-task-by")', 'Bora Korunanoğlu', '$bystander_mail', 'Duyuru', 'Merhaba Bora, imza: ' || $name,
            '<p>imza: ' || $name || '</p>', 'sent', 1);
    -- Sends to the person from the bystander: one sent, one failed, one still pending.
    INSERT INTO mail_tasks (id, sent_by, template_id, body_variables)
    VALUES ('$(uuid_of "$p-task-to")', '$bystander', '$tpl', jsonb_build_object('ad', '${P_FIRST[$p]}', 'adres', $school));
    INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, body_html, status, attempts, error, next_attempt_at)
    VALUES ('$(uuid_of "$p-task-to")', $name, $school, 'Hoş geldin ' || $name, 'Sevgili ' || $name || ', kaydın alındı.',
            '<p>' || $school || '</p>', 'sent', 1, NULL, now()),
           ('$(uuid_of "$p-task-to")', $name, $mixed, 'Hoş geldin', 'Sevgili ' || $name, NULL, 'failed', 3,
            'mailbox ' || $personal || ' unavailable', now()),
           ('$(uuid_of "$p-task-to")', $name, $personal, 'Hatırlatma', 'Sevgili ' || $name, NULL, 'pending', 0, NULL, now() + interval '7 days');
    -- A Mail onayı the person submitted, to the person's own address.
    INSERT INTO mail_approvals (id, submitter_sub, submitter_name, submitter_email, state, template_id, template_version_id,
                                body_variables, created_at, submitted_at, deadline_at, updated_at)
    VALUES ('$(uuid_of "$p-approval")', '$subject', $name, $school, 'pending', '$tpl', '$ver',
            jsonb_build_object('adres', $school), now(), now(), now() + interval '7 days', now());
    INSERT INTO mail_approval_recipients (approval_id, position, email, full_name)
    VALUES ('$(uuid_of "$p-approval")', 1, $personal, $name);
    INSERT INTO mail_approval_events (approval_id, seq, kind, actor_sub, actor_name, note, created_at)
    VALUES ('$(uuid_of "$p-approval")', 1, 'submitted', '$subject', $name, 'Lütfen onaylayın: ' || $name, now());" >/dev/null
}

seed_cms_person() {
  local p=$1 subject=${P_ID[$1]} name
  name=$(full_name "$p")
  pg skylab_cms "
    INSERT INTO collection_items (\"Id\", \"CollectionKey\", \"Slug\", \"Data\", \"UpdatedBy\", \"IsArchived\", \"CreatedAt\", \"UpdatedAt\", \"Version\")
    VALUES ('$(uuid_of "$p-news")', 'News', 'harness-haber-$p',
            jsonb_build_object('title', 'Harness haberi $p', 'author', $(sql_quote "$name"), 'body', 'Kulüp haberi.'),
            '$subject', false, now() - interval '1 day', now() - interval '1 day', 3);
    INSERT INTO collection_items (\"Id\", \"CollectionKey\", \"Slug\", \"Data\", \"UpdatedBy\", \"IsArchived\", \"ArchivedAt\", \"ArchivedBy\",
                                  \"CreatedAt\", \"UpdatedAt\", \"Version\")
    VALUES ('$(uuid_of "$p-news-archived")', 'News', 'harness-arsiv-$p', jsonb_build_object('title', 'Arşiv $p'),
            '$subject', true, now(), '$subject', now() - interval '2 days', now() - interval '2 days', 1);
    INSERT INTO content_blocks (\"Id\", \"ClientId\", \"Slug\", \"BlockPath\", \"BlockType\", \"Value\", \"SortOrder\", \"UpdatedBy\",
                                \"IsArchived\", \"CreatedAt\", \"UpdatedAt\", \"Version\")
    VALUES ('$(uuid_of "$p-block")', 'skylab-site', 'harness', 'hero.$p', 'text', '\"Harness\"'::jsonb, 1, '$subject',
            false, now(), now(), 1);" >/dev/null
  cms_redis SET "draft:skylab-site:$subject:harness" '{"blocks":[]}' EX 172800 >/dev/null
  cms_redis SET "cd:item:News:harness-haber-$p:$subject" '{"title":"taslak"}' EX 604800 >/dev/null
}

seed_forms_person() {
  local p=$1 subject=${P_ID[$1]} name school personal
  name=$(sql_quote "$(full_name "$p")") school=$(sql_quote "${P_SCHOOL[$p]}")
  personal=$(sql_quote "$(tr 'a-z' 'A-Z' <<<"${P_PERSONAL[$p]}")")
  pg forms_db "
    INSERT INTO forms (id, title, owned_by) VALUES ('$(uuid_of "$p-form")', 'Harness formu $p', '$subject');
    INSERT INTO forms (id, title, owned_by) VALUES ('$(uuid_of shared-form)', 'Ortak form', '${P_ID[p8]}') ON CONFLICT DO NOTHING;
    INSERT INTO responses (id, form_id, user_id, data, review_note) VALUES
      ('$(uuid_of "$p-response")', '$(uuid_of shared-form)', '$subject', jsonb_build_object('ad', $name, 'eposta', $school), 'Not: ' || $name),
      ('$(uuid_of "$p-guest-response")', '$(uuid_of shared-form)', NULL, jsonb_build_object('eposta', $personal, 'ad', $name), NULL);
    INSERT INTO responses (id, form_id, user_id, data, reviewed_by) VALUES
      ('$(uuid_of "$p-reviewed")', '$(uuid_of "$p-form")', '${P_ID[p8]}', jsonb_build_object('ad', 'Bora Korunanoğlu'), '$subject');
    INSERT INTO collaborators (form_id, user_id, role) VALUES
      ('$(uuid_of "$p-form")', '$subject', 'Owner'),
      ('$(uuid_of shared-form)', '$subject', 'Editor');" >/dev/null
}

seed_all() {
  local p
  log "seeding: core JIT users, then every service's rows for ${PERSONS[*]}"
  seed_core_shared
  seed_skymail_shared
  for p in "${PERSONS[@]}"; do
    seed_core_jit "$p"
  done
  for p in "${PERSONS[@]}"; do
    seed_core_person "$p"
    seed_skymail_person "$p"
    seed_cms_person "$p"
    seed_forms_person "$p"
  done
  log "seeded"
}

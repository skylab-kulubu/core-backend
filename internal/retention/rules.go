package retention

import "time"

const day = 24 * time.Hour

// Retention periods of core's rules (ADR-0062). They are code, not
// configuration: the policy text names them, and a change is a new rule
// version.
const (
	// GuestPhonePeriod: a guest's phone, the Event's end + 90 days, consent
	// or not.
	GuestPhonePeriod = 90 * day
	// GuestIdentityPeriod: a guest's name and address (with their
	// certificate's address), the person's latest Event's end + 2 years,
	// unless they hold an active invitation consent.
	GuestIdentityPeriod = 2 * 365 * day
	// DoorStaffPeriod: an Event's door staff, its end + 90 days.
	DoorStaffPeriod = 90 * day
	// HitPersonalFieldsPeriod: a short-link click's address, user agent,
	// account and full referer, one year; the row stays.
	HitPersonalFieldsPeriod = 365 * day
	// ReadLinkIPPeriod: the address of an open of a private Media's read
	// link, one year; the record stays three (ReadLinkRecordRetention).
	ReadLinkIPPeriod = 365 * day
)

// Schema is what a run reads from the database before it builds the rules.
type Schema struct {
	// ContactConsents is true once contact_consents exists (core-backend
	// #188). guest_identity then keeps the guests who hold an active
	// invitation consent; before it, no one can hold one.
	ContactConsents bool
}

// Rules are every rule of the sweep, in the order a run takes them. A rule
// is added here and nowhere else; its record, metrics and alarm follow.
func Rules(c Config, s Schema) []Rule {
	return append(sweepRules(s), auditRules(c)...)
}

// eventEndSQL is when the Event e ended, the anchor of "Event + N"
// (ADR-0062): its end date, else its last day's end, else its start. NULL
// when it has none of them: a row anchored on it is counted, never changed.
func eventEndSQL(e string) string {
	return `COALESCE(` + e + `.end_date, (SELECT max(rd.end_date) FROM event_days rd WHERE rd.event_id = ` + e + `.id), ` + e + `.start_date)`
}

// eventEndsSQL is every Event with when it ended, as (id, ended).
var eventEndsSQL = `(SELECT ee.id, ` + eventEndSQL("ee") + ` AS ended FROM events ee)`

// ticketEventEndSQL is when the Event of Ticket t ended.
var ticketEventEndSQL = `(SELECT ` + eventEndSQL("te") + ` FROM events te WHERE te.id = t.event_id)`

// guestTicketSQL: a guest's Ticket. Guest data is what a Ticket without an
// owner holds, the rule account erasure follows too.
const guestTicketSQL = `t.owner_id IS NULL`

// guestIdentityHeldSQL: the Ticket still holds the guest's identity, or one
// of its certificates without an owner still holds their address.
const guestIdentityHeldSQL = `(t.guest_first_name <> '' OR t.guest_last_name <> '' OR t.guest_email <> '' OR t.guest_phone_number <> ''
	OR EXISTS (SELECT 1 FROM certificates gc WHERE gc.ticket_id = t.id AND gc.owner_id IS NULL AND gc.recipient_email <> ''))`

// guestPersonSQL is a guest Ticket's person: its normalized address. Guest
// apply stores addresses trimmed and lowercased; older rows may not be.
const guestPersonSQL = `lower(btrim(t.guest_email))`

// guestPeopleSQL groups the guest Tickets with an address by person, with
// when each of their Events ended. guestPeopleDueSQL keeps the people whose
// every Event can be dated and whose latest one ended before the cutoff $1;
// guestPeopleAnchorlessSQL the people with an Event that cannot be dated.
// Each is computed once per statement, not per Ticket.
var (
	guestPeopleSQL = `SELECT lower(btrim(pt.guest_email)) FROM tickets pt JOIN ` + eventEndsSQL + ` pe ON pe.id = pt.event_id
		WHERE pt.owner_id IS NULL AND btrim(pt.guest_email) <> ''
		GROUP BY lower(btrim(pt.guest_email))`
	guestPeopleDueSQL        = guestPeopleSQL + ` HAVING bool_and(pe.ended IS NOT NULL) AND max(pe.ended) < $1`
	guestPeopleAnchorlessSQL = guestPeopleSQL + ` HAVING NOT bool_and(pe.ended IS NOT NULL)`
)

// guestPendingCertificateSQL: a certificate is still being issued for the
// Ticket, from the guest's name; the Ticket waits for it.
const guestPendingCertificateSQL = `EXISTS (SELECT 1 FROM certificate_jobs gj WHERE gj.ticket_id = t.id AND gj.status IN ('queued', 'running'))`

// guestInvitationConsentSQL: the guest's address holds an active (confirmed,
// open) invitation consent given for the address (contact_consents,
// ADR-0062). An account's own consent is the account's: its guest Tickets do
// not need their copy of the name for it.
const guestInvitationConsentSQL = `EXISTS (SELECT 1 FROM contact_consents gcc
	WHERE gcc.purpose = 'event_invitations' AND gcc.user_id IS NULL AND gcc.ended_at IS NULL
	  AND gcc.confirmed_at IS NOT NULL AND gcc.email = ` + guestPersonSQL + `)`

// refererOriginSQL reduces a referer to its origin, scheme://host,
// lowercased, without user information, port, path, query or fragment; ""
// for anything that is not an absolute URL. It is its own fixed point, so a
// reduced referer is never reduced again.
func refererOriginSQL(col string) string {
	return `CASE WHEN ` + col + ` ~ '^[A-Za-z][A-Za-z0-9+.-]*://' THEN lower(substring(` + col + ` FROM '^([A-Za-z][A-Za-z0-9+.-]*://)')) || lower(COALESCE(substring(` + col + ` FROM '^[A-Za-z][A-Za-z0-9+.-]*://(?:[^/?#]*@)?(\[[^]/?#]*\]|[^/?#:@]*)'), '')) ELSE '' END`
}

func sweepRules(s Schema) []Rule {
	guestIdentityDue := guestTicketSQL + ` AND ` + guestIdentityHeldSQL + `
	AND NOT ` + guestPendingCertificateSQL + `
	AND CASE WHEN btrim(t.guest_email) = '' THEN ` + ticketEventEndSQL + ` < $1
		ELSE ` + guestPersonSQL + ` IN (` + guestPeopleDueSQL + `) END`
	if s.ContactConsents {
		guestIdentityDue += `
	AND NOT ` + guestInvitationConsentSQL
	}
	return []Rule{
		{
			Name: "guest_phone", Version: 1, Kind: KindSweep, Action: ActionScrub,
			Table: "tickets", alias: "t", key: "id", Period: GuestPhonePeriod,
			where:      guestTicketSQL + ` AND t.guest_phone_number <> '' AND ` + ticketEventEndSQL + ` < $1`,
			set:        `guest_phone_number = '', updated_at = now()`,
			anchorless: guestTicketSQL + ` AND t.guest_phone_number <> '' AND ` + ticketEventEndSQL + ` IS NULL`,
		},
		{
			Name: "guest_identity", Version: 1, Kind: KindSweep, Action: ActionScrub,
			Table: "tickets", RelatedTable: "certificates", alias: "t", key: "id", Period: GuestIdentityPeriod,
			where: guestIdentityDue,
			set:   `guest_first_name = '', guest_last_name = '', guest_email = '', guest_phone_number = '', updated_at = now()`,
			// The certificate keeps its recipient name, serial and PDF: the
			// verification record (ADR-0062). Only its address goes.
			related: `UPDATE certificates gc SET recipient_email = '' FROM batch
				WHERE gc.ticket_id = batch.k AND gc.owner_id IS NULL AND gc.recipient_email <> ''`,
			anchorless: guestTicketSQL + ` AND ` + guestIdentityHeldSQL + `
	AND CASE WHEN btrim(t.guest_email) = '' THEN ` + ticketEventEndSQL + ` IS NULL
		ELSE ` + guestPersonSQL + ` IN (` + guestPeopleAnchorlessSQL + `) END`,
		},
		{
			Name: "door_staff", Version: 1, Kind: KindSweep, Action: ActionDelete,
			Table: "event_door_staff", alias: "s", key: "ctid", Period: DoorStaffPeriod,
			where:      `(SELECT ` + eventEndSQL("se") + ` FROM events se WHERE se.id = s.event_id) < $1`,
			anchorless: `(SELECT ` + eventEndSQL("se") + ` FROM events se WHERE se.id = s.event_id) IS NULL`,
		},
		{
			Name: "url_hits_scrub", Version: 1, Kind: KindSweep, Action: ActionScrub,
			Table: "url_hits", alias: "h", key: "id", Period: HitPersonalFieldsPeriod,
			where: `h.at < $1 AND (h.ip <> '' OR h.user_agent <> '' OR h.user_id IS NOT NULL OR h.referer <> ` + refererOriginSQL("h.referer") + `)`,
			set:   `ip = '', user_agent = '', user_id = NULL, referer = ` + refererOriginSQL("h.referer"),
		},
		{
			Name: "read_link_ip", Version: 1, Kind: KindSweep, Action: ActionScrub,
			Table: "media_read_link_opens", alias: "o", key: "ctid", Period: ReadLinkIPPeriod,
			where: `o.opened_at < $1 AND o.client_ip <> ''`,
			set:   `client_ip = ''`,
		},
	}
}

// auditRules count what core's hourly cleanups should already have removed,
// a day past their window. They change nothing in any mode.
func auditRules(c Config) []Rule {
	hitsAge := Rule{
		Name: "url_hits_age", Version: 1, Kind: KindAudit, Action: ActionCount,
		Table: "url_hits", alias: "h", Period: hitDeletionWindow + overdueGrace, Alarm: true,
		where: `h.at < $1`,
	}
	if !HourlyHitDeletion(c.Mode) {
		hitsAge.NotApplicable = "click rows are kept in apply mode; url_hits_scrub empties their personal fields"
	}
	readLinks := ReadLinkWindow(c.Mode) + overdueGrace
	return []Rule{
		hitsAge,
		{
			Name: "read_links_age", Version: 1, Kind: KindAudit, Action: ActionCount,
			Table: "media_read_links", alias: "l", Period: readLinks, Alarm: true,
			where: `l.issued_at < $1`,
		},
		{
			Name: "read_link_opens_age", Version: 1, Kind: KindAudit, Action: ActionCount,
			Table: "media_read_link_opens", alias: "o", Period: readLinks, Alarm: true,
			where: `o.opened_at < $1`,
		},
		{
			// The snapshot cleanup deletes SkyMail's list, then the row.
			Name: "mail_snapshots_age", Version: 1, Kind: KindAudit, Action: ActionCount,
			Table: "event_mail_snapshots", alias: "m", Period: overdueGrace, Alarm: true,
			where: `m.expires_at < $1`,
		},
		{
			// An archived Media still used by a record keeps its object
			// (media-lifecycle.md), so these rows alone are no alarm.
			Name: "media_archived_objects", Version: 1, Kind: KindAudit, Action: ActionCount,
			Table: "media", alias: "md", Period: c.MediaRecoveryWindow + overdueGrace,
			where: `md.deleted_at IS NOT NULL AND md.deleted_at < $1 AND md.blob_purged_at IS NULL`,
		},
		{
			Name: "media_expired_objects", Version: 1, Kind: KindAudit, Action: ActionCount,
			Table: "media", alias: "md", Period: overdueGrace,
			where: `md.expires_at < $1 AND md.blob_purged_at IS NULL AND (md.deleted_at IS NULL OR md.blob_purge_started_at IS NOT NULL)`,
		},
	}
}

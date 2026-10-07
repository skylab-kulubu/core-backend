package consent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Mailer sends the confirmation mail of a pending grant through SkyMail.
// Sending is best effort: a mail that does not go leaves the grant pending.
// The mail greets nobody by name: whoever typed the address may have typed
// any name with it.
type Mailer interface {
	ConsentConfirmation(ctx context.Context, templateKey, recipient string, vars map[string]string)
}

// AddressSource reads a person's addresses: what Keycloak holds (Primary,
// School and Personal e-mail) with core's row, as account erasure reads them
// but without its limit of three (account.NewPersonAddresses). An error
// carries no address.
type AddressSource interface {
	ErasureAddresses(context.Context, uuid.UUID) ([]string, error)
}

// Service keeps contact consents in PostgreSQL.
type Service struct {
	pool   *pgxpool.Pool
	config Config
	mail   Mailer
	// addresses reads a person's addresses for their own list and
	// withdrawal; nil reads core's row alone.
	addresses AddressSource
	now       func() time.Time
	// mailTimeout bounds a confirmation mail, which is sent after the
	// request that asked for it has been answered.
	mailTimeout time.Duration
	// async runs a confirmation mail; tests replace it to wait for it.
	async func(func())
}

// NewService builds the consent service. mail may be nil: pending grants
// then wait without a confirmation mail.
func NewService(pool *pgxpool.Pool, config Config, mail Mailer) *Service {
	return &Service{
		pool: pool, config: config, mail: mail,
		now:         func() time.Time { return time.Now().UTC() },
		mailTimeout: 20 * time.Second,
		async:       func(fn func()) { go fn() },
	}
}

// Enabled reports whether grants are recorded (KeyEnv is set).
func (s *Service) Enabled() bool { return s != nil && s.config.Enabled }

// Config is the service's settings.
func (s *Service) Config() Config { return s.config }

// SetClock replaces the clock (tests and tools).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// SetMailer sets the confirmation mail's sender (startup builds SkyMail
// after the service).
func (s *Service) SetMailer(mail Mailer) { s.mail = mail }

// SetAddresses sets where a person's addresses are read for their own list
// and withdrawal (Keycloak with core's row). Without it core's row alone.
func (s *Service) SetAddresses(addresses AddressSource) { s.addresses = addresses }

// SetAsync replaces how a confirmation mail is run (tests).
func (s *Service) SetAsync(async func(func())) { s.async = async }

// ServiceSource is the source a service client speaks for, if any.
func (s *Service) ServiceSource(client string) (Source, bool) {
	source, ok := s.config.ServiceSources[client]
	return source, ok
}

// Grant is one consent a person gave.
type Grant struct {
	Purpose     Purpose
	TextVersion string
	// The subject: UserID for a signed-in person consenting for
	// themselves, else Email.
	UserID uuid.UUID
	Email  string
	Source Source
	// ClientID is the Keycloak client of the app the person used (the
	// token's azp), "" without a token.
	ClientID string
	// EventID is the Event the consent was given on (Guest apply).
	EventID *uuid.UUID
	// EmailVerified: the calling product showed that the address is the
	// person's (a code sent to it, a verified sign-in). Core believes it
	// only from a client of VerifiedClientsEnv; any other grant for an
	// address waits for the person's confirmation from a link.
	EmailVerified bool
}

// GrantResult is what Grant did.
type GrantResult struct {
	ID     uuid.UUID
	Status Status
	// Created is false when an open grant was already there and stays.
	Created bool
}

// Validate checks a grant and fills its text version and normalized
// address. Grant runs it too; Guest apply runs it before it writes a Ticket.
func (g *Grant) Validate() error {
	if err := g.Purpose.check(); err != nil {
		return err
	}
	if !g.Purpose.Allows(g.Source) {
		return fmt.Errorf("%w: purpose %s cannot be given through %s", ErrInvalid, g.Purpose, g.Source)
	}
	if g.TextVersion == "" {
		g.TextVersion, _ = CurrentText(g.Purpose)
	}
	if !KnownText(g.Purpose, g.TextVersion) {
		return ErrUnknownText
	}
	if g.ClientID != "" && !clientIDPattern.MatchString(g.ClientID) {
		g.ClientID = ""
	}
	if g.UserID != uuid.Nil {
		if g.Email != "" {
			return fmt.Errorf("%w: a person consents for their account, not an address", ErrInvalid)
		}
		return nil
	}
	email, ok := NormalizeEmail(g.Email)
	if !ok {
		return fmt.Errorf("%w: email", ErrInvalid)
	}
	g.Email = email
	return nil
}

// Grant records a consent. A grant for the account of the signed-in person,
// or for an address a client of VerifiedClientsEnv verified, is active at
// once; any other waits for the person to confirm it from the link core
// mails to the address (double opt-in): whoever calls Guest apply may type
// anybody's address, and a staff member must not tick the box for someone
// else.
//
// An open grant of the subject for the purpose is kept as it is, with two
// exceptions. A verified grant meeting a pending one ends it as superseded
// and is recorded as a row of its own, so each row's evidence is what its
// own request said. An unverified grant meeting a pending one mails the
// confirmation again, at most once a day and three times in all.
func (s *Service) Grant(ctx context.Context, g Grant) (GrantResult, error) {
	if !s.Enabled() {
		return GrantResult{}, ErrDisabled
	}
	if err := g.Validate(); err != nil {
		return GrantResult{}, err
	}
	verified := g.UserID != uuid.Nil || (g.EmailVerified && s.config.VerifiedClients[g.ClientID])
	for attempt := 1; ; attempt++ {
		result, mail, err := s.grantOnce(ctx, g, verified)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && attempt < grantAttempts {
			// A concurrent grant for the subject (a double submit) wrote
			// the open row since the read: read it again, after a short
			// wait that grows with every try.
			select {
			case <-time.After(time.Duration(attempt*attempt) * 5 * time.Millisecond):
			case <-ctx.Done():
				return GrantResult{}, ctx.Err()
			}
			continue
		}
		if err != nil {
			return GrantResult{}, err
		}
		if mail {
			s.sendConfirmation(result.ID, g)
		}
		return result, nil
	}
}

func (s *Service) grantOnce(ctx context.Context, g Grant, verified bool) (GrantResult, bool, error) {
	now := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return GrantResult{}, false, err
	}
	defer tx.Rollback(ctx)

	var subject string
	var args []any
	var emailHMAC []byte
	if g.UserID != uuid.Nil {
		subject, args = "user_id = $2", []any{g.Purpose, g.UserID}
	} else {
		emailHMAC = s.config.emailHMAC(g.Email)
		subject, args = "email_hmac = $2", []any{g.Purpose, emailHMAC}
	}
	var (
		id          uuid.UUID
		confirmedAt *time.Time
		sentAt      *time.Time
		mails       int
	)
	err = tx.QueryRow(ctx, `
		SELECT id, confirmed_at, confirmation_sent_at, confirmation_mails FROM contact_consents
		WHERE purpose = $1 AND `+subject+` AND ended_at IS NULL
		FOR UPDATE`, args...).Scan(&id, &confirmedAt, &sentAt, &mails)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return GrantResult{}, false, err
	case confirmedAt != nil:
		return GrantResult{ID: id, Status: StatusActive}, false, tx.Commit(ctx)
	case verified:
		// A product verified the address the pending grant waits on: the
		// pending row ends, and this request's own evidence is a new row.
		if _, err := tx.Exec(ctx, `
			UPDATE contact_consents
			SET ended_at = $2, ended_reason = 'superseded', ended_via = 'service', email = NULL
			WHERE id = $1`, id, now); err != nil {
			return GrantResult{}, false, err
		}
	default:
		mail := mails < MaxConfirmationMails && (sentAt == nil || now.Sub(*sentAt) >= ConfirmationResend)
		if mail {
			if _, err := tx.Exec(ctx, `
				UPDATE contact_consents SET confirmation_sent_at = $2, confirmation_mails = confirmation_mails + 1
				WHERE id = $1`, id, now); err != nil {
				return GrantResult{}, false, err
			}
		}
		return GrantResult{ID: id, Status: StatusPending}, mail, tx.Commit(ctx)
	}

	id = uuid.New()
	var (
		confirmedVia *string
		confirmed    *time.Time
		sent         *time.Time
		email        *string
		userID       *uuid.UUID
	)
	mails = 0
	switch {
	case g.UserID != uuid.Nil:
		via := "account"
		confirmedVia, confirmed, userID = &via, &now, &g.UserID
	case verified:
		via := "service"
		confirmedVia, confirmed = &via, &now
	default:
		sent, mails = &now, 1
	}
	if g.UserID == uuid.Nil {
		email = &g.Email
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO contact_consents (
			id, purpose, user_id, email, email_hmac, source, client_id, text_version, event_id,
			granted_at, confirmed_at, confirmed_via, confirmation_sent_at, confirmation_mails
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		id, g.Purpose, userID, email, emailHMAC, g.Source, g.ClientID, g.TextVersion, g.EventID,
		now, confirmed, confirmedVia, sent, mails,
	); err != nil {
		return GrantResult{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return GrantResult{}, false, err
	}
	if confirmed != nil {
		return GrantResult{ID: id, Status: StatusActive, Created: true}, false, nil
	}
	return GrantResult{ID: id, Status: StatusPending, Created: true}, true, nil
}

// sendConfirmation mails the confirm link of a pending grant, after the
// request that recorded it. It names nobody: no name travels with it.
func (s *Service) sendConfirmation(id uuid.UUID, g Grant) {
	if s.mail == nil || s.config.ConfirmTemplateKey == "" {
		log.Printf("contact consent: confirmation mail not sent (SkyMail or %s not configured); the grant stays pending", ConfirmTemplateKeyEnv)
		return
	}
	now := s.now()
	vars := map[string]string{
		"confirmUrl":  s.config.confirmURL(id, now.Add(PendingTTL)),
		"withdrawUrl": s.config.WithdrawURL(id),
		"purpose":     string(g.Purpose),
	}
	recipient, template := g.Email, s.config.ConfirmTemplateKey
	s.async(func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.mailTimeout)
		defer cancel()
		s.mail.ConsentConfirmation(ctx, template, recipient, vars)
	})
}

// ErrDisabled is any consent operation while KeyEnv is unset.
var ErrDisabled = errors.New("contact consent: off (" + KeyEnv + " is not set)")

// ErrAddressesUnavailable is a person's own list or withdrawal while their
// addresses cannot be read (Keycloak unreachable, core's row unreadable):
// a passing failure, to be tried again.
var ErrAddressesUnavailable = errors.New("contact consent: the person's addresses cannot be read now")

// LinkOutcome is what a link did.
type LinkOutcome string

const (
	// OutcomeConfirmed: a pending grant became active.
	OutcomeConfirmed LinkOutcome = "confirmed"
	// OutcomeRenewed: an active grant was renewed.
	OutcomeRenewed LinkOutcome = "renewed"
	// OutcomeEnded: the grant the link was for has ended; a confirm link
	// does not reopen it.
	OutcomeEnded LinkOutcome = "ended"
	// OutcomeSuperseded: the pending grant the link was for was replaced by
	// a grant a product verified; there is nothing left to confirm.
	OutcomeSuperseded LinkOutcome = "superseded"
	// OutcomeWithdrawn: the subject's open grant ended now.
	OutcomeWithdrawn LinkOutcome = "withdrawn"
	// OutcomeNothingOpen: the subject has no open grant (already
	// withdrawn, expired or erased). Withdrawing again is not an error.
	OutcomeNothingOpen LinkOutcome = "nothing_open"
)

// LinkGrant is what a link's page tells the person before they act: the
// purpose and the text of the grant it names, and where that grant is.
type LinkGrant struct {
	Purpose     Purpose
	TextVersion string
	// Status is pending, active, or how it ended.
	Status Status
}

// ConfirmLinkGrant reads the grant a confirm link names, for its page. An
// expired or forged link is an error; a grant no longer kept is
// ErrNoSubject.
func (s *Service) ConfirmLinkGrant(ctx context.Context, token string) (LinkGrant, error) {
	return s.linkGrant(ctx, token, linkConfirm)
}

// WithdrawLinkGrant reads the grant a withdraw link names, for its page.
// The page asks before it acts: a mail scanner opening the link must not
// withdraw.
func (s *Service) WithdrawLinkGrant(ctx context.Context, token string) (LinkGrant, error) {
	return s.linkGrant(ctx, token, linkWithdraw)
}

func (s *Service) linkGrant(ctx context.Context, token string, kind linkKind) (LinkGrant, error) {
	if !s.Enabled() {
		return LinkGrant{}, ErrDisabled
	}
	id, err := s.config.verify(token, kind, s.now())
	if err != nil {
		return LinkGrant{}, err
	}
	var grant LinkGrant
	var confirmed bool
	var reason *string
	err = s.pool.QueryRow(ctx, `SELECT purpose, text_version, confirmed_at IS NOT NULL, ended_reason FROM contact_consents WHERE id = $1`, id).
		Scan(&grant.Purpose, &grant.TextVersion, &confirmed, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return LinkGrant{}, ErrNoSubject
	}
	if err != nil {
		return LinkGrant{}, err
	}
	grant.Status = status(confirmed, reason)
	return grant, nil
}

func status(confirmed bool, reason *string) Status {
	switch {
	case reason == nil && confirmed:
		return StatusActive
	case reason == nil:
		return StatusPending
	case *reason == "expired":
		return StatusExpired
	case *reason == "superseded":
		return StatusSuperseded
	}
	return StatusWithdrawn
}

// Confirm runs a confirm link: a pending grant becomes active (the person
// showed the address is theirs), an active one is renewed. A link of an ended
// grant does nothing: a withdrawal stands until the person grants again.
func (s *Service) Confirm(ctx context.Context, token string) (LinkOutcome, error) {
	if !s.Enabled() {
		return "", ErrDisabled
	}
	now := s.now()
	id, err := s.config.verify(token, linkConfirm, now)
	if err != nil {
		return "", err
	}
	var outcome LinkOutcome
	err = s.pool.QueryRow(ctx, `
		WITH target AS (
			SELECT id, confirmed_at, ended_at, ended_reason FROM contact_consents WHERE id = $1 FOR UPDATE
		), changed AS (
			UPDATE contact_consents c
			SET confirmed_at = COALESCE(c.confirmed_at, $2),
				confirmed_via = COALESCE(c.confirmed_via, 'link'),
				renewed_at = CASE WHEN c.confirmed_at IS NULL THEN c.renewed_at ELSE $2 END,
				renewal_requested_at = CASE WHEN c.confirmed_at IS NULL THEN c.renewal_requested_at ELSE NULL END
			FROM target
			WHERE c.id = target.id AND target.ended_at IS NULL
			RETURNING c.id
		)
		SELECT CASE
			WHEN target.ended_reason = 'superseded' THEN 'superseded'
			WHEN target.ended_at IS NOT NULL THEN 'ended'
			WHEN target.confirmed_at IS NULL THEN 'confirmed'
			ELSE 'renewed'
		END
		FROM target`, id, now).Scan(&outcome)
	if errors.Is(err, pgx.ErrNoRows) {
		// Deleted: an erased account, or a row past its time.
		return "", ErrNoSubject
	}
	return outcome, err
}

// Withdraw runs a withdraw link: every open grant (pending or active) of the
// link's purpose that mails the same address ends (mailedTo). The link names
// a grant, but it withdraws what is open now, so a link from an old
// invitation still works after the person granted again, and the link of an
// account's grant also ends a grant given for the account's address (and
// back, even once the address grant has ended and kept only its HMAC): one
// click ends the mails, whichever grant a mail was sent on. Repeating it is
// harmless.
func (s *Service) Withdraw(ctx context.Context, token string, via WithdrawVia) (LinkOutcome, error) {
	if !s.Enabled() {
		return "", ErrDisabled
	}
	now := s.now()
	id, err := s.config.verify(token, linkWithdraw, now)
	if err != nil {
		return "", err
	}
	if via != WithdrawByPage && via != WithdrawByOneClick {
		return "", fmt.Errorf("%w: withdraw via %q", ErrInvalid, via)
	}
	var (
		purpose   Purpose
		userID    *uuid.UUID
		email     *string
		emailHMAC []byte
	)
	err = s.pool.QueryRow(ctx, `SELECT purpose, user_id, email, email_hmac FROM contact_consents WHERE id = $1`, id).
		Scan(&purpose, &userID, &email, &emailHMAC)
	if errors.Is(err, pgx.ErrNoRows) {
		// Deleted: erased with the account, or proof past its time.
		return OutcomeNothingOpen, nil
	}
	if err != nil {
		return "", err
	}
	var target mailedTo
	if userID != nil {
		// The account's own grant mails the account's address in core.
		if target, err = s.accountTarget(ctx, *userID, false); err != nil {
			return "", err
		}
	} else {
		target.hmacs = [][]byte{emailHMAC}
		if email != nil {
			target.addresses = []string{*email}
		}
	}
	n, err := s.endOpen(ctx, purpose, target, string(via), now)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return OutcomeNothingOpen, nil
	}
	return OutcomeWithdrawn, nil
}

// mailedTo names the grants whose mails go to a person's addresses: the
// account's own grant (users), the grants given for one of the addresses (by
// their HMACs, which outlive the address), and the own grants of any account
// whose address is one of them (in clear, or by HMAC when only that is left).
type mailedTo struct {
	users     []uuid.UUID
	addresses []string
	hmacs     [][]byte
}

// endOpen ends the open grants of purpose that target names, as withdrawn
// by via. An ended grant keeps no address.
func (s *Service) endOpen(ctx context.Context, purpose Purpose, target mailedTo, via string, now time.Time) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	users, err := s.accountsMailing(ctx, tx, purpose, target)
	if err != nil {
		return 0, err
	}
	hmacs := target.hmacs
	if hmacs == nil {
		hmacs = [][]byte{}
	}
	tag, err := tx.Exec(ctx, `
		UPDATE contact_consents c
		SET ended_at = $4, ended_reason = 'withdrawn', ended_via = $5, email = NULL
		WHERE c.purpose = $1 AND c.ended_at IS NULL AND (c.user_id = ANY($2) OR c.email_hmac = ANY($3))`,
		purpose, users, hmacs, now, via)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), tx.Commit(ctx)
}

// accountsMailing is target's accounts and the accounts with an open own
// grant of purpose whose address in core (email or school_email) is one of
// target's, matched in clear or by HMAC. Only the accounts that hold an open
// own grant are read, so the work follows the members who consented, not
// every account.
func (s *Service) accountsMailing(ctx context.Context, tx pgx.Tx, purpose Purpose, target mailedTo) ([]uuid.UUID, error) {
	out := append([]uuid.UUID{}, target.users...)
	if len(target.addresses) == 0 && len(target.hmacs) == 0 {
		return out, nil
	}
	wanted := map[string]bool{}
	for _, sum := range target.hmacs {
		wanted[string(sum)] = true
	}
	for _, address := range target.addresses {
		if email, ok := NormalizeEmail(address); ok {
			wanted[string(s.config.emailHMAC(email))] = true
		}
	}
	rows, err := tx.Query(ctx, `
		SELECT u.id, u.email, u.school_email
		FROM contact_consents c JOIN users u ON u.id = c.user_id
		WHERE c.purpose = $1 AND c.ended_at IS NULL AND c.user_id IS NOT NULL`, purpose)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var email, school string
		if err := rows.Scan(&id, &email, &school); err != nil {
			return nil, err
		}
		for _, raw := range []string{email, school} {
			if normalized, ok := NormalizeEmail(raw); ok && wanted[string(s.config.emailHMAC(normalized))] {
				out = append(out, id)
				break
			}
		}
	}
	return out, rows.Err()
}

// renewalAnchor is when a grant last showed the person still wants it: its
// confirmation, its last renewal, or the person's last check-in at an Event
// (the subject's Tickets: the account's own, or the guest Tickets of the
// address). A grant whose anchor is older than RenewalAfter is due its
// renewal question. The retention sweep reads the same expression.
const RenewalAnchorSQL = `GREATEST(c.confirmed_at, c.renewed_at, CASE
	WHEN c.user_id IS NULL THEN (
		SELECT max(ci.created_at) FROM tickets t JOIN ticket_checkins ci ON ci.ticket_id = t.id
		WHERE t.owner_id IS NULL AND t.guest_email = c.email AND t.guest_email <> '')
	ELSE (
		SELECT max(ci.created_at) FROM tickets t JOIN ticket_checkins ci ON ci.ticket_id = t.id
		WHERE t.owner_id = c.user_id)
	END)`

// RenewalQuestionSQL is a grant's renewal question while it stands, else
// NULL: asked, and asked no earlier than the anchor (RenewalAnchorSQL). A
// question older than the last confirmation, renewal or attendance was
// answered by it: coming to an Event says "keep inviting me" as well as the
// link does. Confirming or renewing clears the question; attending cannot,
// so the question is read through this everywhere: a void question is not
// shown, does not stop a new one once the grant is due again, and never
// ends the grant (the retention sweep's consent_renewal_unanswered).
const RenewalQuestionSQL = `CASE WHEN c.renewal_requested_at >= ` + RenewalAnchorSQL + ` THEN c.renewal_requested_at END`

// AudienceEntry is one address SkyMail may send a purpose's mail to.
type AudienceEntry struct {
	// ID is the grant: the cursor, and what a renewal request names.
	ID    uuid.UUID `json:"id"`
	Email string    `json:"email"`
	// Subject is "account" for a person's own grant (the address is their
	// account's, read now), "address" for a grant given for an address.
	Subject     string    `json:"subject"`
	ConfirmedAt time.Time `json:"confirmedAt"`
	// WithdrawURL goes in the mail's footer and in its List-Unsubscribe
	// header, with List-Unsubscribe-Post: List-Unsubscribe=One-Click.
	WithdrawURL string `json:"withdrawUrl"`
	// RenewalDue: nothing showed for RenewalAfter that the person still
	// wants these mails. SkyMail sends the renewal question with RenewURL
	// instead of an invitation, then reports it (RequestRenewal).
	RenewalDue bool `json:"renewalDue"`
	// RenewalRequestedAt is when the standing renewal question was asked
	// (RenewalQuestionSQL); absent when none stands, a question the person
	// answered by attending included.
	RenewalRequestedAt *time.Time `json:"renewalRequestedAt,omitempty"`
	RenewURL           string     `json:"renewUrl,omitempty"`
}

// MaxAudiencePage is the largest audience page.
const MaxAudiencePage = 1000

// Audience lists the confirmed, open grants of a purpose in id order, from
// after (exclusive; uuid.Nil from the start). next is uuid.Nil on the last
// page. A grant whose account has no address now (anonymized) is left out.
func (s *Service) Audience(ctx context.Context, purpose Purpose, after uuid.UUID, limit int) ([]AudienceEntry, uuid.UUID, error) {
	if !s.Enabled() {
		return nil, uuid.Nil, ErrDisabled
	}
	if err := purpose.check(); err != nil {
		return nil, uuid.Nil, err
	}
	if limit <= 0 || limit > MaxAudiencePage {
		limit = MaxAudiencePage
	}
	now := s.now()
	rows, err := s.pool.Query(ctx, `
		SELECT c.id, COALESCE(c.email, u.email, ''), c.user_id IS NOT NULL, c.confirmed_at,
			`+RenewalQuestionSQL+`, `+RenewalAnchorSQL+` < $4
		FROM contact_consents c
		LEFT JOIN users u ON u.id = c.user_id AND u.account_state = 'active'
		WHERE c.purpose = $1 AND c.ended_at IS NULL AND c.confirmed_at IS NOT NULL AND c.id > $2
		ORDER BY c.id
		LIMIT $3`, purpose, after, limit+1, now.Add(-RenewalAfter))
	if err != nil {
		return nil, uuid.Nil, err
	}
	defer rows.Close()
	var out []AudienceEntry
	var last uuid.UUID
	more := false
	for n := 0; rows.Next(); n++ {
		if n == limit {
			more = true
			break
		}
		var entry AudienceEntry
		var member bool
		if err := rows.Scan(&entry.ID, &entry.Email, &member, &entry.ConfirmedAt, &entry.RenewalRequestedAt, &entry.RenewalDue); err != nil {
			return nil, uuid.Nil, err
		}
		last = entry.ID
		if strings.TrimSpace(entry.Email) == "" {
			continue
		}
		entry.Email = strings.ToLower(strings.TrimSpace(entry.Email))
		entry.Subject = "address"
		if member {
			entry.Subject = "account"
		}
		entry.WithdrawURL = s.config.WithdrawURL(entry.ID)
		if entry.RenewalDue {
			entry.RenewURL = s.config.confirmURL(entry.ID, now.Add(RenewalLinkTTL))
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, uuid.Nil, err
	}
	if !more {
		last = uuid.Nil
	}
	return out, last, nil
}

// RequestRenewal records that SkyMail sent the renewal question for the
// named grants. Only open, confirmed grants of purpose that are due and have
// no standing question change (a question answered by attending stands no
// more, RenewalQuestionSQL). RenewalAnswerWindow later the retention sweep
// ends the ones nobody renewed.
func (s *Service) RequestRenewal(ctx context.Context, purpose Purpose, ids []uuid.UUID) (int64, error) {
	if !s.Enabled() {
		return 0, ErrDisabled
	}
	if err := purpose.check(); err != nil {
		return 0, err
	}
	now := s.now()
	tag, err := s.pool.Exec(ctx, `
		UPDATE contact_consents c SET renewal_requested_at = $3
		WHERE c.id = ANY($2) AND c.purpose = $1 AND c.ended_at IS NULL AND c.confirmed_at IS NOT NULL
		  AND (`+RenewalQuestionSQL+`) IS NULL AND `+RenewalAnchorSQL+` < $4`,
		purpose, ids, now, now.Add(-RenewalAfter))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// Lookup tells a product the state of its users' grants for a purpose, by
// address: active, pending, or absent from the map (none open). It counts
// every grant that mails the address (mailedTo): one given for it (by its
// HMAC), and the own grant of an active account whose email or school_email
// it is. Forms reads it to know which rejected applications it may keep,
// Place and Guessr which players' addresses.
//
// Each way in is its own branch of one UNION ALL, joined on equality, so
// the work grows with the addresses asked and the rows that match, not with
// their product. It stops at lookupTimeout, and earlier when ctx ends.
func (s *Service) Lookup(ctx context.Context, purpose Purpose, emails []string) (map[string]Status, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	if err := purpose.check(); err != nil {
		return nil, err
	}
	var addresses []string
	var hmacs [][]byte
	seen := map[string]bool{}
	for _, raw := range emails {
		email, ok := NormalizeEmail(raw)
		if !ok {
			return nil, fmt.Errorf("%w: email", ErrInvalid)
		}
		if !seen[email] {
			seen[email] = true
			addresses = append(addresses, email)
			hmacs = append(hmacs, s.config.emailHMAC(email))
		}
	}
	out := map[string]Status{}
	if len(addresses) == 0 {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	rows, err := s.pool.Query(ctx, `
		WITH asked AS (
			SELECT address, hmac FROM unnest($2::text[], $3::bytea[]) AS a(address, hmac)
		), open AS (
			SELECT a.address, c.confirmed_at IS NOT NULL AS active
			FROM asked a
			JOIN contact_consents c ON c.email_hmac = a.hmac
			WHERE c.purpose = $1 AND c.ended_at IS NULL
			UNION ALL
			SELECT a.address, c.confirmed_at IS NOT NULL
			FROM asked a
			JOIN users u ON lower(btrim(u.email)) = a.address
			JOIN contact_consents c ON c.user_id = u.id
			WHERE u.account_state = 'active' AND c.purpose = $1 AND c.ended_at IS NULL
			UNION ALL
			SELECT a.address, c.confirmed_at IS NOT NULL
			FROM asked a
			JOIN users u ON lower(btrim(u.school_email)) = a.address
			JOIN contact_consents c ON c.user_id = u.id
			WHERE u.account_state = 'active' AND c.purpose = $1 AND c.ended_at IS NULL
		)
		SELECT address, bool_or(active) FROM open GROUP BY address`, purpose, addresses, hmacs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var address string
		var active bool
		if err := rows.Scan(&address, &active); err != nil {
			return nil, err
		}
		out[address] = StatusPending
		if active {
			out[address] = StatusActive
		}
	}
	return out, rows.Err()
}

// WithdrawForAddress ends, on a product's word (the person unticked the box
// in the product), every open grant of a purpose that mails the address.
func (s *Service) WithdrawForAddress(ctx context.Context, purpose Purpose, rawEmail string) (bool, error) {
	if !s.Enabled() {
		return false, ErrDisabled
	}
	if err := purpose.check(); err != nil {
		return false, err
	}
	email, ok := NormalizeEmail(rawEmail)
	if !ok {
		return false, fmt.Errorf("%w: email", ErrInvalid)
	}
	n, err := s.endOpen(ctx, purpose, mailedTo{addresses: []string{email}, hmacs: [][]byte{s.config.emailHMAC(email)}}, viaService, s.now())
	return n > 0, err
}

// Own is one of a signed-in person's grants.
type Own struct {
	ID          uuid.UUID  `json:"id"`
	Purpose     Purpose    `json:"purpose"`
	Status      Status     `json:"status"`
	Subject     string     `json:"subject"`
	Source      Source     `json:"source"`
	TextVersion string     `json:"textVersion"`
	GrantedAt   time.Time  `json:"grantedAt"`
	ConfirmedAt *time.Time `json:"confirmedAt,omitempty"`
	EndedAt     *time.Time `json:"endedAt,omitempty"`
}

// ownSubject is the rows of a person: their account's grants and the
// grants given for one of their addresses.
const ownSubject = `(c.user_id = $1 OR c.email_hmac = ANY($2))`

// accountTarget is the account and its addresses: with identity, every
// address the person holds (Keycloak's Primary, School and Personal e-mail
// with core's row, as erasure reads them, when SetAddresses gave a source);
// without, core's row, the address an account's own grant mails.
func (s *Service) accountTarget(ctx context.Context, userID uuid.UUID, identity bool) (mailedTo, error) {
	target := mailedTo{users: []uuid.UUID{userID}}
	var raw []string
	if identity && s.addresses != nil {
		addresses, err := s.addresses.ErasureAddresses(ctx, userID)
		if err != nil {
			return mailedTo{}, fmt.Errorf("%w: %w", ErrAddressesUnavailable, err)
		}
		raw = addresses
	} else {
		var email, school string
		err := s.pool.QueryRow(ctx, `SELECT email, school_email FROM users WHERE id = $1`, userID).Scan(&email, &school)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return mailedTo{}, err
		default:
			raw = []string{email, school}
		}
	}
	for _, address := range raw {
		if normalized, ok := NormalizeEmail(address); ok && !slices.Contains(target.addresses, normalized) {
			target.addresses = append(target.addresses, normalized)
			target.hmacs = append(target.hmacs, s.config.emailHMAC(normalized))
		}
	}
	return target, nil
}

func (s *Service) hmacs(addresses []string) [][]byte {
	var out [][]byte
	seen := map[string]bool{}
	for _, raw := range addresses {
		if email, ok := NormalizeEmail(raw); ok && !seen[email] {
			seen[email] = true
			out = append(out, s.config.emailHMAC(email))
		}
	}
	return out
}

// Mine lists a signed-in person's grants, ended ones included, newest first:
// their account's and those given for any of their addresses, Keycloak's
// Personal e-mail included.
func (s *Service) Mine(ctx context.Context, userID uuid.UUID) ([]Own, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	target, err := s.accountTarget(ctx, userID, true)
	if err != nil {
		return nil, err
	}
	hmacs := target.hmacs
	if hmacs == nil {
		hmacs = [][]byte{}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT c.id, c.purpose, c.user_id IS NOT NULL, c.source, c.text_version, c.granted_at, c.confirmed_at,
			c.ended_at, c.ended_reason
		FROM contact_consents c
		WHERE `+ownSubject+`
		ORDER BY c.granted_at DESC, c.id`, userID, hmacs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Own{}
	for rows.Next() {
		var own Own
		var account bool
		var reason *string
		if err := rows.Scan(&own.ID, &own.Purpose, &account, &own.Source, &own.TextVersion, &own.GrantedAt,
			&own.ConfirmedAt, &own.EndedAt, &reason); err != nil {
			return nil, err
		}
		own.Subject = "address"
		if account {
			own.Subject = "account"
		}
		own.Status = status(own.ConfirmedAt != nil, reason)
		out = append(out, own)
	}
	return out, rows.Err()
}

// WithdrawMine ends a signed-in person's open grants of a purpose: their
// account's, and every grant that mails one of their addresses, Keycloak's
// Personal e-mail included. While their addresses cannot be read it ends at
// once what core reaches without them (the account's own grant and the
// grants for core's row's addresses) and returns how many with
// ErrAddressesUnavailable: a grant for another of their addresses may still
// be open, so the person tries again.
func (s *Service) WithdrawMine(ctx context.Context, userID uuid.UUID, purpose Purpose) (int64, error) {
	if !s.Enabled() {
		return 0, ErrDisabled
	}
	if err := purpose.check(); err != nil {
		return 0, err
	}
	target, err := s.accountTarget(ctx, userID, true)
	if errors.Is(err, ErrAddressesUnavailable) {
		known, knownErr := s.accountTarget(ctx, userID, false)
		if knownErr != nil {
			return 0, knownErr
		}
		n, endErr := s.endOpen(ctx, purpose, known, viaSelf, s.now())
		if endErr != nil {
			return n, endErr
		}
		return n, err
	}
	if err != nil {
		return 0, err
	}
	return s.endOpen(ctx, purpose, target, viaSelf, s.now())
}

// EraseSubject deletes every grant of a person, open or ended: their
// account's and those given for any of addresses (account erasure, ADR-0051
// and ADR-0062; there is no suppression list). It works with consents off
// too, by account and by address, so an erasure never waits on the key; the
// proof rows of ended address grants are then left to their own time.
func (s *Service) EraseSubject(ctx context.Context, userID uuid.UUID, addresses []string) (int64, error) {
	var hmacs [][]byte
	var emails []string
	for _, raw := range addresses {
		if email, ok := NormalizeEmail(raw); ok {
			emails = append(emails, email)
		}
	}
	if s.Enabled() {
		hmacs = s.hmacs(emails)
	}
	if emails == nil {
		emails = []string{}
	}
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM contact_consents
		WHERE user_id = $1 OR email = ANY($2) OR email_hmac = ANY($3)`, userID, emails, hmacs)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

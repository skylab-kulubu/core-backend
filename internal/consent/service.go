package consent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Mailer sends the confirmation mail of a pending grant through SkyMail.
// Sending is best effort: a mail that does not go leaves the grant pending,
// and the person's next grant for the address sends it again.
type Mailer interface {
	ConsentConfirmation(ctx context.Context, templateKey, recipient, fullName string, vars map[string]string)
}

// Service keeps contact consents in PostgreSQL.
type Service struct {
	pool   *pgxpool.Pool
	config Config
	mail   Mailer
	now    func() time.Time
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
	// person's (a code sent to it, a verified sign-in). Without it the
	// grant waits for the person's confirmation from a link.
	EmailVerified bool
	// Name is only the greeting of the confirmation mail; it is not kept.
	Name string
}

// GrantResult is what Grant did.
type GrantResult struct {
	ID     uuid.UUID
	Status Status
	// Created is false when an open grant was already there.
	Created bool
}

// validate checks a grant and fills its text version and address.
func (g *Grant) validate() error {
	if _, ok := purposes[g.Purpose]; !ok {
		return fmt.Errorf("%w: purpose", ErrInvalid)
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
// or for an address the calling product verified, is active at once; any
// other waits for the person to confirm it from the link core mails to the
// address (double opt-in): whoever calls Guest apply may type anybody's
// address, and a staff member must not tick the box for someone else.
//
// An open grant of the subject for the purpose is kept as it is: repeating a
// grant changes nothing, a verified grant confirms a pending one, and an
// unverified one sends the confirmation mail again (at most every ten
// minutes).
func (s *Service) Grant(ctx context.Context, g Grant) (GrantResult, error) {
	if !s.Enabled() {
		return GrantResult{}, ErrDisabled
	}
	if err := g.validate(); err != nil {
		return GrantResult{}, err
	}
	for attempt := 0; ; attempt++ {
		result, mail, err := s.grantOnce(ctx, g)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && attempt == 0 {
			// A concurrent grant for the subject (a double submit) wrote
			// the open row since the read: read it again.
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

func (s *Service) grantOnce(ctx context.Context, g Grant) (GrantResult, bool, error) {
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
	)
	err = tx.QueryRow(ctx, `
		SELECT id, confirmed_at, confirmation_sent_at FROM contact_consents
		WHERE purpose = $1 AND `+subject+` AND ended_at IS NULL
		FOR UPDATE`, args...).Scan(&id, &confirmedAt, &sentAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		id = uuid.New()
		var confirmedVia *string
		var confirmed, sent *time.Time
		switch {
		case g.UserID != uuid.Nil:
			via := "account"
			confirmedVia, confirmed = &via, &now
		case g.EmailVerified:
			via := "service"
			confirmedVia, confirmed = &via, &now
		default:
			sent = &now
		}
		var email *string
		if g.UserID == uuid.Nil {
			email = &g.Email
		}
		var userID *uuid.UUID
		if g.UserID != uuid.Nil {
			userID = &g.UserID
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO contact_consents (
				id, purpose, user_id, email, email_hmac, source, client_id, text_version, event_id,
				granted_at, confirmed_at, confirmed_via, confirmation_sent_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			id, g.Purpose, userID, email, emailHMAC, g.Source, g.ClientID, g.TextVersion, g.EventID,
			now, confirmed, confirmedVia, sent,
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
	case err != nil:
		return GrantResult{}, false, err
	}

	if confirmedAt != nil {
		return GrantResult{ID: id, Status: StatusActive}, false, tx.Commit(ctx)
	}
	if g.EmailVerified {
		if _, err := tx.Exec(ctx, `
			UPDATE contact_consents SET confirmed_at = $2, confirmed_via = 'service' WHERE id = $1`, id, now); err != nil {
			return GrantResult{}, false, err
		}
		return GrantResult{ID: id, Status: StatusActive}, false, tx.Commit(ctx)
	}
	mail := sentAt == nil || now.Sub(*sentAt) >= confirmationResend
	if mail {
		if _, err := tx.Exec(ctx, `UPDATE contact_consents SET confirmation_sent_at = $2 WHERE id = $1`, id, now); err != nil {
			return GrantResult{}, false, err
		}
	}
	return GrantResult{ID: id, Status: StatusPending}, mail, tx.Commit(ctx)
}

// sendConfirmation mails the confirm link of a pending grant, after the
// request that recorded it.
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
	recipient, name, template := g.Email, strings.TrimSpace(g.Name), s.config.ConfirmTemplateKey
	s.async(func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.mailTimeout)
		defer cancel()
		s.mail.ConsentConfirmation(ctx, template, recipient, name, vars)
	})
}

// ErrDisabled is any consent operation while KeyEnv is unset.
var ErrDisabled = errors.New("contact consent: off (" + KeyEnv + " is not set)")

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
	// OutcomeWithdrawn: the subject's open grant ended now.
	OutcomeWithdrawn LinkOutcome = "withdrawn"
	// OutcomeNothingOpen: the subject has no open grant (already
	// withdrawn, expired or erased). Withdrawing again is not an error.
	OutcomeNothingOpen LinkOutcome = "nothing_open"
)

// CheckWithdrawLink reports whether token is a withdraw link core signed.
// The page asks before it acts: a mail scanner opening the link must not
// withdraw.
func (s *Service) CheckWithdrawLink(token string) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	_, err := s.config.verify(token, linkWithdraw, s.now())
	return err
}

// CheckConfirmLink reports whether token is a confirm link core signed that
// has not expired.
func (s *Service) CheckConfirmLink(token string) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	_, err := s.config.verify(token, linkConfirm, s.now())
	return err
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
			SELECT id, confirmed_at, ended_at FROM contact_consents WHERE id = $1 FOR UPDATE
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
			WHEN target.ended_at IS NOT NULL THEN 'ended'
			WHEN target.confirmed_at IS NULL THEN 'confirmed'
			ELSE 'renewed'
		END
		FROM target`, id, now).Scan(&outcome)
	if errors.Is(err, pgx.ErrNoRows) {
		// Deleted: a pending grant left unconfirmed, an erased account, or
		// proof past its time.
		return "", ErrNoSubject
	}
	return outcome, err
}

// Withdraw runs a withdraw link: every open grant (pending or active) of
// the link's subject for the link's purpose ends. The link names a grant, but
// it withdraws the subject's current one, so a link from an old invitation
// still works after the person granted again. Repeating it is harmless.
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
	tag, err := s.pool.Exec(ctx, `
		UPDATE contact_consents c
		SET ended_at = $2, ended_reason = 'withdrawn', ended_via = $3,
			email = CASE WHEN c.user_id IS NULL THEN NULL ELSE c.email END
		FROM contact_consents link
		WHERE link.id = $1
		  AND c.purpose = link.purpose
		  AND c.ended_at IS NULL
		  AND (c.user_id = link.user_id OR c.email_hmac = link.email_hmac)`, id, now, string(via))
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return OutcomeNothingOpen, nil
	}
	return OutcomeWithdrawn, nil
}

// renewalAnchor is when a grant last showed the person still wants it: its
// confirmation, its last renewal, or the person's last check-in at an Event
// (the subject's Tickets: the account's own, or the guest Tickets of the
// address). A grant whose anchor is older than RenewalAfter is due its
// renewal question. The retention sweep reads the same expression.
const RenewalAnchorSQL = `GREATEST(c.confirmed_at, c.renewed_at, CASE
	WHEN c.user_id IS NULL THEN (
		SELECT max(ci.created_at) FROM tickets t JOIN ticket_checkins ci ON ci.ticket_id = t.id
		WHERE t.owner_id IS NULL AND t.guest_email = c.email)
	ELSE (
		SELECT max(ci.created_at) FROM tickets t JOIN ticket_checkins ci ON ci.ticket_id = t.id
		WHERE t.owner_id = c.user_id)
	END)`

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
	RenewalDue         bool       `json:"renewalDue"`
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
	if _, ok := purposes[purpose]; !ok {
		return nil, uuid.Nil, fmt.Errorf("%w: purpose", ErrInvalid)
	}
	if limit <= 0 || limit > MaxAudiencePage {
		limit = MaxAudiencePage
	}
	now := s.now()
	rows, err := s.pool.Query(ctx, `
		SELECT c.id, COALESCE(c.email, u.email, ''), c.user_id IS NOT NULL, c.confirmed_at,
			c.renewal_requested_at, `+RenewalAnchorSQL+` < $4
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
// named grants. Only open, confirmed grants of purpose that are due and not
// already asked change. RenewalAnswerWindow later the retention sweep ends
// the ones nobody renewed.
func (s *Service) RequestRenewal(ctx context.Context, purpose Purpose, ids []uuid.UUID) (int64, error) {
	if !s.Enabled() {
		return 0, ErrDisabled
	}
	now := s.now()
	tag, err := s.pool.Exec(ctx, `
		UPDATE contact_consents c SET renewal_requested_at = $3
		WHERE c.id = ANY($2) AND c.purpose = $1 AND c.ended_at IS NULL AND c.confirmed_at IS NOT NULL
		  AND c.renewal_requested_at IS NULL AND `+RenewalAnchorSQL+` < $4`,
		purpose, ids, now, now.Add(-RenewalAfter))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// Lookup tells a product the state of its users' grants for a purpose, by
// address: active, pending, or absent from the map (none open). Forms reads
// it to know which rejected applications it may keep.
func (s *Service) Lookup(ctx context.Context, purpose Purpose, emails []string) (map[string]Status, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	byHMAC := map[string]string{}
	var hmacs [][]byte
	for _, raw := range emails {
		email, ok := NormalizeEmail(raw)
		if !ok {
			return nil, fmt.Errorf("%w: email", ErrInvalid)
		}
		sum := s.config.emailHMAC(email)
		if _, seen := byHMAC[string(sum)]; !seen {
			byHMAC[string(sum)] = email
			hmacs = append(hmacs, sum)
		}
	}
	out := map[string]Status{}
	if len(hmacs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT email_hmac, confirmed_at IS NOT NULL FROM contact_consents
		WHERE purpose = $1 AND email_hmac = ANY($2) AND ended_at IS NULL`, purpose, hmacs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sum []byte
		var active bool
		if err := rows.Scan(&sum, &active); err != nil {
			return nil, err
		}
		status := StatusPending
		if active {
			status = StatusActive
		}
		out[byHMAC[string(sum)]] = status
	}
	return out, rows.Err()
}

// WithdrawForAddress ends the open grant of an address for a purpose on a
// product's word: the person unticked the box in the product.
func (s *Service) WithdrawForAddress(ctx context.Context, purpose Purpose, rawEmail string) (bool, error) {
	if !s.Enabled() {
		return false, ErrDisabled
	}
	if _, ok := purposes[purpose]; !ok {
		return false, fmt.Errorf("%w: purpose", ErrInvalid)
	}
	email, ok := NormalizeEmail(rawEmail)
	if !ok {
		return false, fmt.Errorf("%w: email", ErrInvalid)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE contact_consents
		SET ended_at = $3, ended_reason = 'withdrawn', ended_via = 'service', email = NULL
		WHERE purpose = $1 AND email_hmac = $2 AND ended_at IS NULL`,
		purpose, s.config.emailHMAC(email), s.now())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
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
// grants given for one of their account's addresses.
const ownSubject = `(c.user_id = $1 OR c.email_hmac = ANY($2))`

func (s *Service) accountHMACs(ctx context.Context, userID uuid.UUID) ([][]byte, error) {
	var email, school string
	err := s.pool.QueryRow(ctx, `SELECT email, school_email FROM users WHERE id = $1`, userID).Scan(&email, &school)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.hmacs([]string{email, school}), nil
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

// Mine lists a signed-in person's grants, ended ones included, newest first.
func (s *Service) Mine(ctx context.Context, userID uuid.UUID) ([]Own, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	hmacs, err := s.accountHMACs(ctx, userID)
	if err != nil {
		return nil, err
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
		switch {
		case reason != nil && *reason == "expired":
			own.Status = StatusExpired
		case reason != nil:
			own.Status = StatusWithdrawn
		case own.ConfirmedAt != nil:
			own.Status = StatusActive
		default:
			own.Status = StatusPending
		}
		out = append(out, own)
	}
	return out, rows.Err()
}

// WithdrawMine ends a signed-in person's open grants of a purpose: their
// account's and their addresses'.
func (s *Service) WithdrawMine(ctx context.Context, userID uuid.UUID, purpose Purpose) (int64, error) {
	if !s.Enabled() {
		return 0, ErrDisabled
	}
	if _, ok := purposes[purpose]; !ok {
		return 0, fmt.Errorf("%w: purpose", ErrInvalid)
	}
	hmacs, err := s.accountHMACs(ctx, userID)
	if err != nil {
		return 0, err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE contact_consents c
		SET ended_at = $4, ended_reason = 'withdrawn', ended_via = 'self',
			email = CASE WHEN c.user_id IS NULL THEN NULL ELSE c.email END
		WHERE `+ownSubject+` AND c.purpose = $3 AND c.ended_at IS NULL`, userID, hmacs, purpose, s.now())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
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

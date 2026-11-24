package commpref

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/consent"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The Form respondents audience (ADR-0075 karar 6; sky_lab_genel
// .scratch/forms-posta-core bilet 07; docs/news-audience.md "Form
// respondents"): the people who answered a form, read from Forms when the
// send is resolved, never kept. The sender's right comes from Forms (their
// role on the form) or from Keycloak (Forms' service administrator role);
// the purpose decides who of them the mail may go to.

// AudienceFormRespondents is the people who answered one form.
const AudienceFormRespondents AudienceType = "form_respondents"

// FormRespondentsEnv turns the audience on: "on"; unset or "off" leaves it
// off, and a request of the type is refused (audience_type_not_enabled).
const FormRespondentsEnv = "FORMS_RESPONDENTS_AUDIENCE"

// FormRespondentsFromEnv reads FormRespondentsEnv.
func FormRespondentsFromEnv(getenv func(string) string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(getenv(FormRespondentsEnv))) {
	case "", "off":
		return false, nil
	case "on":
		return true, nil
	}
	return false, fmt.Errorf("%s must be on or off", FormRespondentsEnv)
}

// FormsAdminRole is Forms' service administrator role (forms-backend
// Authentication:Roles:PlatformAdmin): its holder may mail any form's
// respondents. Forms reads it from the token's resource_access under its
// clients, forms and the legacy dotnet, ignoring case; core reads the
// person's effective roles of the same clients from Keycloak, since Forms'
// service token cannot see the sender's roles (ADR-0075 sapma 6).
const FormsAdminRole = "skyforms:*"

// FormsRoleClients are the clients FormsAdminRole counts under.
var FormsRoleClients = []string{"forms", "dotnet"}

// Purpose is why a send goes (ADR-0072 karar 2 and 8).
type Purpose string

const (
	// PurposeTransactional (islem): about the recipient's own application.
	// Everyone who answered gets it; no preference or consent is read.
	PurposeTransactional Purpose = "islem"
	// PurposeAnnouncement (duyuru): each recipient passes their own filter.
	PurposeAnnouncement Purpose = "duyuru"
)

// The sender's role on the form, as Forms says it.
const (
	FormRoleOwner  = "owner"
	FormRoleEditor = "editor"
	FormRoleViewer = "viewer"
	FormRoleNone   = "none"
)

// MaxFormRespondents bounds the respondents one resolution reads from
// Forms: more is a form core will not mail in one send.
const MaxFormRespondents = 20000

var (
	// ErrFormUnknown: Forms has no such form.
	ErrFormUnknown = errors.New("communication preference: unknown form")
	// ErrFormForbidden: the sender has no role on the form and no
	// FormsAdminRole.
	ErrFormForbidden = errors.New("communication preference: form not allowed")
	// ErrFormsUnavailable: Forms (or Keycloak's role read) did not answer.
	ErrFormsUnavailable = errors.New("communication preference: forms unavailable")
	// ErrTypeNotEnabled: the audience type is off.
	ErrTypeNotEnabled = errors.New("communication preference: audience type not enabled")
	// ErrFormRespondentsTooMany: over MaxFormRespondents.
	ErrFormRespondentsTooMany = errors.New("communication preference: too many form respondents")
)

// Respondent is one answer's person as Forms gives it: an account's
// subject, or a guest's name and address (raw; core checks the address).
type Respondent struct {
	UserID     uuid.UUID
	GuestName  string
	GuestEmail string
}

// RespondentsPage is one page of Forms' respondents.
type RespondentsPage struct {
	// ActorRole is the sender's role on the form: owner, editor, viewer or
	// none.
	ActorRole     string
	ResponseCount int
	Items         []Respondent
	// Next is Forms' cursor of the next page; empty on the last.
	Next string
}

// FormRespondentsSource reads a form's respondents from Forms
// (GET /internal/v1/forms/{formId}/respondents?actor=). A form Forms does
// not know is ErrFormUnknown; anything that did not answer is an error.
type FormRespondentsSource interface {
	Respondents(ctx context.Context, formID, actor uuid.UUID, after string, limit int) (RespondentsPage, error)
}

// ClientRoles reads a person's effective roles of a Keycloak client
// (identity.Directory.EffectiveClientRoles).
type ClientRoles interface {
	EffectiveClientRoles(ctx context.Context, userID uuid.UUID, clientID string) ([]string, error)
}

// ConsentLookup reads Contact consents by address (consent.Service.Lookup).
type ConsentLookup interface {
	Lookup(ctx context.Context, purpose consent.Purpose, emails []string) (map[string]consent.Status, error)
}

// FormAudit is one resolution of a form's respondents that a send or an
// approval used: who, which form, when, why and how many. Never an address
// or a name.
type FormAudit struct {
	ActorSub    uuid.UUID
	FormID      uuid.UUID
	At          time.Time
	Purpose     Purpose
	Category    string
	Promotional bool
	Recipients  int
	Client      string
}

// FormAuditLog keeps FormAudit rows (form_audience_audits).
type FormAuditLog interface {
	RecordFormAudience(ctx context.Context, a FormAudit) error
}

// FormRespondentsRequest is one form send's audience question.
type FormRespondentsRequest struct {
	FormID   uuid.UUID
	ActorSub uuid.UUID
	Purpose  Purpose
	// Category is the duyuru send's ("club:duyuru", "team:<id>:duyuru"…);
	// empty for islem.
	Category    string
	Promotional bool
	// Client is the caller's client id, for the audit row.
	Client string
}

// FormRecipient is one person a form send goes to.
type FormRecipient struct {
	// UserID is the account's; nil for a guest.
	UserID *uuid.UUID `json:"userId,omitempty"`
	Email  string     `json:"email"`
	Name   string     `json:"name"`
	// Via is always "respondent".
	Via string `json:"via"`
	// UnsubscribeURL turns the category off: a Member's duyuru send only.
	// A guest's opt-out is SkyMail's own record (ADR-0072 karar 4).
	UnsubscribeURL string `json:"unsubscribeUrl,omitempty"`
	// Cursor orders the recipients: a keyed digest of the address, so no
	// address travels in a URL.
	Cursor string `json:"-"`
}

// FormRespondentsCounts are the recipients by who they are.
type FormRespondentsCounts struct {
	Members    int `json:"members"`
	NonMembers int `json:"nonMembers"`
	Guests     int `json:"guests"`
	Total      int `json:"total"`
	// Responses is the form's answers Forms counted (archived left out).
	Responses int `json:"responses"`
}

// FormRespondentsSkipped are the respondents left out, each once, under the
// first reason that held: disabled, invalidEmail, duplicateEmail, then
// (duyuru) optedOut, noConsent, noPromotionalConsent.
type FormRespondentsSkipped struct {
	InvalidEmail         int `json:"invalidEmail"`
	Disabled             int `json:"disabled"`
	DuplicateEmail       int `json:"duplicateEmail"`
	OptedOut             int `json:"optedOut"`
	NoConsent            int `json:"noConsent"`
	NoPromotionalConsent int `json:"noPromotionalConsent"`
}

// FormRespondentsPage is one page of the audience.
type FormRespondentsPage struct {
	Counts  FormRespondentsCounts  `json:"counts"`
	Skipped FormRespondentsSkipped `json:"skipped"`
	Items   []FormRecipient        `json:"items,omitempty"`
	// Next is the cursor of the next page, empty on the last.
	Next string `json:"-"`
}

// FormRespondents resolves the Form respondents audience.
type FormRespondents struct {
	audience *Audience
	forms    FormRespondentsSource
	roles    ClientRoles
	consents ConsentLookup
	audit    FormAuditLog
	now      func() time.Time
}

// SetFormRespondents turns the Form respondents audience on over a: forms
// reads the respondents, roles the sender's Forms roles, consents the
// Contact consents, audit keeps the row of each send's resolution. Nil
// leaves it off.
func (a *Audience) SetFormRespondents(f *FormRespondents) {
	if a == nil {
		return
	}
	if f != nil {
		f.audience = a
	}
	a.forms = f
}

// FormRespondentsOn reports whether the Form respondents audience is on.
func (a *Audience) FormRespondentsOn() bool {
	return a != nil && a.forms != nil && a.forms.forms != nil && a.forms.roles != nil && a.forms.consents != nil && a.forms.audit != nil
}

// NewFormRespondents is the audience's reads; SetFormRespondents puts it
// on an Audience.
func NewFormRespondents(forms FormRespondentsSource, roles ClientRoles, consents ConsentLookup, audit FormAuditLog) *FormRespondents {
	return &FormRespondents{forms: forms, roles: roles, consents: consents, audit: audit, now: time.Now}
}

func (r FormRespondentsRequest) check() (Category, bool, error) {
	if r.FormID == uuid.Nil || r.ActorSub == uuid.Nil {
		return Category{}, false, ErrInvalid
	}
	switch r.Purpose {
	case PurposeTransactional:
		// A mail about the recipient's own application has no category and
		// is never a promotion (ADR-0072 karar 8).
		if r.Category != "" || r.Promotional {
			return Category{}, false, ErrInvalid
		}
		return Category{}, false, nil
	case PurposeAnnouncement:
		cat, err := ParseCategory(r.Category)
		if err != nil || (cat.Scope != ScopeClub && cat.Scope != ScopeTeam) {
			return Category{}, false, ErrInvalid
		}
		return cat, true, nil
	}
	return Category{}, false, ErrInvalid
}

// ResolveForm answers one page of a form send's audience: the recipients
// after the cursor after (empty: from the start), at most limit, in cursor
// order; countOnly answers the counts alone. Every page reads every
// respondent from Forms again and recomputes the whole audience, so its
// counts are the whole audience's and nothing is kept between pages.
// Unless countOnly, the first page (after empty) writes the audit row: a
// send's or an approval's resolution starts there.
func (a *Audience) ResolveForm(ctx context.Context, req FormRespondentsRequest, after string, limit int, countOnly bool) (FormRespondentsPage, error) {
	if !a.Enabled() {
		return FormRespondentsPage{}, ErrDisabled
	}
	if !a.FormRespondentsOn() {
		return FormRespondentsPage{}, ErrTypeNotEnabled
	}
	f := a.forms
	cat, announcement, err := req.check()
	if err != nil {
		return FormRespondentsPage{}, err
	}
	if limit <= 0 || limit > MaxAudiencePage {
		limit = MaxAudiencePage
	}
	role, responses, respondents, err := f.readAll(ctx, req.FormID, req.ActorSub)
	if err != nil {
		return FormRespondentsPage{}, err
	}
	if err := f.allowed(ctx, req.ActorSub, role); err != nil {
		return FormRespondentsPage{}, err
	}
	snap, err := a.snapshot(ctx)
	if err != nil {
		return FormRespondentsPage{}, err
	}
	if announcement && cat.Scope == ScopeTeam {
		if _, ok := snap.tree.teams[cat.GroupID]; !ok {
			return FormRespondentsPage{}, ErrUnknownTeam
		}
	}

	var ids []uuid.UUID
	for _, r := range respondents {
		if r.UserID != uuid.Nil && !slices.Contains(ids, r.UserID) {
			ids = append(ids, r.UserID)
		}
	}
	accounts, err := a.accounts.Accounts(ctx, ids)
	if err != nil {
		return FormRespondentsPage{}, err
	}
	settings, err := a.prefs.store.ListPeople(ctx, ids)
	if err != nil {
		return FormRespondentsPage{}, err
	}

	page := FormRespondentsPage{Counts: FormRespondentsCounts{Responses: responses}}
	type candidate struct {
		recipient FormRecipient
		address   string
		member    bool
		guest     bool
	}
	var candidates []candidate
	seen := map[string]bool{}
	for _, r := range respondents {
		var c candidate
		raw := r.GuestEmail
		if r.UserID != uuid.Nil {
			id := r.UserID
			account, stored := accounts[id]
			person, member := snap.people[id]
			if id == user.DeletedSubject || !stored || account.Blocked || (member && !person.Enabled) || person.ServiceAccount {
				page.Skipped.Disabled++
				continue
			}
			// The Primary e-mail: Keycloak's for a Member (as the News
			// audience reads it), core's account for anyone else.
			raw = account.User.Email
			c.recipient.Name = recipientName(person.Person, account)
			if member {
				raw = person.Email
			}
			c.recipient.UserID, c.member = &id, member
		} else {
			c.guest = true
			c.recipient.Name = strings.Join(strings.Fields(r.GuestName), " ")
		}
		address, ok := consent.NormalizeEmail(raw)
		if !ok {
			page.Skipped.InvalidEmail++
			continue
		}
		if seen[address] {
			page.Skipped.DuplicateEmail++
			continue
		}
		seen[address] = true
		c.address = address
		c.recipient.Email = strings.TrimSpace(raw)
		c.recipient.Via = "respondent"
		candidates = append(candidates, c)
	}

	// The consents of everyone who is no Member, read once.
	var invited, promoted map[string]consent.Status
	if announcement {
		var addresses, all []string
		for _, c := range candidates {
			all = append(all, c.address)
			if !c.member {
				addresses = append(addresses, c.address)
			}
		}
		if invited, err = f.lookup(ctx, consent.PurposeEventInvitations, addresses); err != nil {
			return FormRespondentsPage{}, err
		}
		if req.Promotional {
			if promoted, err = f.lookup(ctx, consent.PurposePromotional, all); err != nil {
				return FormRespondentsPage{}, err
			}
		}
	}

	cursorKey := a.prefs.config.cursorKey()
	var recipients []FormRecipient
	for _, c := range candidates {
		if announcement {
			switch {
			case c.member && !snap.tree.ResolveChosen(ExplicitOf(settings[*c.recipient.UserID], ChannelEmail), cat).Effective:
				// ADR-0072 karar 3: the default alone holds nothing back
				// (they entered with their own answer); their own "off" does.
				page.Skipped.OptedOut++
				continue
			case !c.member && invited[c.address] != consent.StatusActive:
				// ADR-0075 karar 6 (Yusuf, 2026-10-26): no Member gets a
				// duyuru without an active Davet onayı.
				page.Skipped.NoConsent++
				continue
			case req.Promotional && promoted[c.address] != consent.StatusActive:
				page.Skipped.NoPromotionalConsent++
				continue
			}
		}
		r := c.recipient
		r.Cursor = formCursor(cursorKey, req.FormID, c.address)
		switch {
		case c.guest:
			page.Counts.Guests++
		case c.member:
			page.Counts.Members++
		default:
			page.Counts.NonMembers++
		}
		recipients = append(recipients, r)
	}
	page.Counts.Total = len(recipients)

	if !countOnly && after == "" {
		if err := f.audit.RecordFormAudience(ctx, FormAudit{
			ActorSub: req.ActorSub, FormID: req.FormID, At: f.now().UTC(), Purpose: req.Purpose,
			Category: req.Category, Promotional: req.Promotional, Recipients: page.Counts.Total, Client: req.Client,
		}); err != nil {
			return FormRespondentsPage{}, err
		}
	}
	if countOnly {
		return page, nil
	}
	slices.SortFunc(recipients, func(x, y FormRecipient) int { return strings.Compare(x.Cursor, y.Cursor) })
	start := 0
	if after != "" {
		start, _ = slices.BinarySearchFunc(recipients, after, func(r FormRecipient, cursor string) int {
			return strings.Compare(r.Cursor, cursor)
		})
		if start < len(recipients) && recipients[start].Cursor == after {
			start++
		}
	}
	end := min(start+limit, len(recipients))
	page.Items = make([]FormRecipient, 0, end-start)
	for _, r := range recipients[start:end] {
		if announcement && r.UserID != nil && snap.people[*r.UserID].ID != uuid.Nil {
			link, err := a.prefs.UnsubscribeURL(*r.UserID, ChannelEmail, []Category{cat})
			if err != nil {
				return FormRespondentsPage{}, err
			}
			r.UnsubscribeURL = link
		}
		page.Items = append(page.Items, r)
	}
	if end < len(recipients) {
		page.Next = recipients[end-1].Cursor
	}
	return page, nil
}

// readAll reads every page of the form's respondents.
func (f *FormRespondents) readAll(ctx context.Context, formID, actor uuid.UUID) (string, int, []Respondent, error) {
	var (
		role      string
		responses int
		out       []Respondent
		cursor    string
	)
	for {
		page, err := f.forms.Respondents(ctx, formID, actor, cursor, MaxAudiencePage)
		switch {
		case errors.Is(err, ErrFormUnknown):
			return "", 0, nil, err
		case err != nil:
			return "", 0, nil, fmt.Errorf("%w: %v", ErrFormsUnavailable, err)
		}
		if role == "" {
			role, responses = page.ActorRole, page.ResponseCount
		}
		out = append(out, page.Items...)
		if len(out) > MaxFormRespondents {
			return "", 0, nil, ErrFormRespondentsTooMany
		}
		if page.Next == "" || page.Next == cursor {
			return role, responses, out, nil
		}
		cursor = page.Next
	}
}

// allowed is Forms' rule (ResponseExports.GetFormMailRecipientsAsync): a
// role on the form, or FormsAdminRole under one of FormsRoleClients.
func (f *FormRespondents) allowed(ctx context.Context, actor uuid.UUID, role string) error {
	switch role {
	case FormRoleOwner, FormRoleEditor, FormRoleViewer:
		return nil
	case FormRoleNone:
	default:
		// A role core does not know is no role.
	}
	for _, client := range FormsRoleClients {
		roles, err := f.roles.EffectiveClientRoles(ctx, actor, client)
		if err != nil {
			// A realm without the legacy client, or a sender it does not
			// know, holds nothing there.
			if isNotFound(err) {
				continue
			}
			return fmt.Errorf("%w: roles: %v", ErrFormsUnavailable, err)
		}
		if slices.ContainsFunc(roles, func(r string) bool { return strings.EqualFold(r, FormsAdminRole) }) {
			return nil
		}
	}
	return ErrFormForbidden
}

func (f *FormRespondents) lookup(ctx context.Context, purpose consent.Purpose, addresses []string) (map[string]consent.Status, error) {
	if len(addresses) == 0 {
		return map[string]consent.Status{}, nil
	}
	out := make(map[string]consent.Status, len(addresses))
	// Lookup takes any number; a chunk keeps one query bounded.
	for chunk := range slices.Chunk(addresses, MaxRecipients) {
		got, err := f.consents.Lookup(ctx, purpose, chunk)
		if err != nil {
			return nil, fmt.Errorf("consent lookup: %w", err)
		}
		for k, v := range got {
			out[k] = v
		}
	}
	return out, nil
}

// isNotFound is a client or a person Keycloak does not have.
func isNotFound(err error) bool {
	return errors.Is(err, identity.ErrNotFound) || errors.Is(err, identity.ErrClientNotFound)
}

// cursorKey is the key of the Form respondents' cursors.
func (c Config) cursorKey() []byte {
	return derive(c.keys[c.current], "form respondents cursor v1")
}

// formCursor is a recipient's place in a form's audience: the keyed digest
// of the form and the address.
func formCursor(key []byte, formID uuid.UUID, address string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(formID.String()))
	mac.Write([]byte{0})
	mac.Write([]byte(address))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

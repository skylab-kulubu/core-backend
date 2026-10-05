// Package consent keeps contact consents (ADR-0062, Davet onayı): a person's
// explicit consent (KVKK art. 5/1) for one purpose: invitations to future SKY
// LAB events, or keeping a team application for future recruitment
// (docs/contact-consents.md).
//
// A grant is recorded, confirmed and ended; it is never reopened. A grant for
// an address nobody has shown to be the person's waits for the person to
// confirm it from a signed link (double opt-in). Withdrawing takes one click
// from a signed link in every invitation, needs no sign-in, and can be
// repeated. Account erasure deletes a person's grants.
package consent

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Purpose is what a person consents to be contacted for.
type Purpose string

// Purposes (ADR-0062). A new one needs its own text, a migration widening
// contact_consents_purpose_check and an entry in purposes.
const (
	// PurposeEventInvitations is invitations to SKY LAB's future events by
	// e-mail.
	PurposeEventInvitations Purpose = "event_invitations"
	// PurposeRecruitmentPool is keeping a team application that was not
	// accepted, to be considered in future recruitment (Forms' second box).
	// Not enabled: whether it covers an address or one application is not
	// agreed with Forms yet.
	PurposeRecruitmentPool Purpose = "recruitment_pool"
)

// purposeSpec is what core accepts for a purpose.
type purposeSpec struct {
	// texts are the consent texts the purpose may be given on: the version
	// codes a client may send, the newest last. A code is the "Sürüm" of
	// the Açık Rıza Metni (sky_lab_genel notes/hukuki); a new wording is a
	// new code here.
	texts []string
	// sources are the apps a grant of the purpose may come from.
	sources []Source
	// enabled: core records and serves the purpose. A purpose that is not
	// enabled is refused everywhere (ErrPurposeNotEnabled).
	enabled bool
	// label is what the person consents to, in Turkish, as the pages name
	// it.
	label string
}

var purposes = map[Purpose]purposeSpec{
	PurposeEventInvitations: {
		texts:   []string{"davet-v1"},
		sources: []Source{SourceGuestApply, SourceForms, SourcePlace, SourceGuessr, SourceSelf},
		enabled: true,
		label:   "SKY LAB'ın gelecek etkinliklerine davet e-postaları",
	},
	PurposeRecruitmentPool: {
		texts:   []string{"alim-havuzu-v1"},
		sources: []Source{SourceForms},
		enabled: false,
		label:   "ekip başvurunun gelecek alımlarda değerlendirilmek üzere saklanması",
	},
}

// texts are the box texts of every version, as the person read them next to
// the box (Turkish). The confirm page shows the one the grant was given on.
var texts = map[string]string{
	"davet-v1":       "SKY LAB'ın gelecek etkinliklerine davet e-postası almak istiyorum. Bunun için adımı ve e-posta adresimi saklayabilirsiniz. İstediğim zaman her davetteki bağlantıyla vazgeçebilirim.",
	"alim-havuzu-v1": "Bu dönem kabul edilmezsem başvurumun gelecek alımlarda değerlendirilmek üzere saklanmasını istiyorum. İstediğim zaman vazgeçebilirim.",
}

// Text is the box text of a version, "" for a version core does not know.
func Text(version string) string { return texts[version] }

// Label is what a purpose's grant is for, in Turkish.
func (p Purpose) Label() string { return purposes[p].label }

// Allows reports whether a grant of purpose p may come from source s.
func (p Purpose) Allows(s Source) bool {
	for _, allowed := range purposes[p].sources {
		if allowed == s {
			return true
		}
	}
	return false
}

// CurrentText is the newest text version of a purpose: what a grant that
// names only the purpose was given on.
func CurrentText(p Purpose) (string, bool) {
	texts := purposes[p].texts
	if len(texts) == 0 {
		return "", false
	}
	return texts[len(texts)-1], true
}

// KnownText reports whether version is a text of purpose p.
func KnownText(p Purpose, version string) bool {
	for _, known := range purposes[p].texts {
		if known == version {
			return true
		}
	}
	return false
}

// ParsePurpose reads a purpose a client sent: ErrInvalid for one core does
// not know, ErrPurposeNotEnabled for one it knows but does not take yet.
func ParsePurpose(raw string) (Purpose, error) {
	p := Purpose(strings.TrimSpace(raw))
	return p, p.check()
}

func (p Purpose) check() error {
	spec, ok := purposes[p]
	switch {
	case !ok:
		return fmt.Errorf("%w: purpose", ErrInvalid)
	case !spec.enabled:
		return ErrPurposeNotEnabled
	}
	return nil
}

// EnabledPurposes is how many purposes core takes: the most a Guest apply
// consents list may name.
func EnabledPurposes() int {
	n := 0
	for _, spec := range purposes {
		if spec.enabled {
			n++
		}
	}
	return n
}

// Source is the app a grant came from.
type Source string

const (
	// SourceGuestApply is the consents field of Guest apply
	// (docs/guest-apply.md), whoever called it.
	SourceGuestApply Source = "guest_apply"
	// SourceForms, SourcePlace and SourceGuessr are those products' service
	// accounts (ServiceSourcesEnv).
	SourceForms  Source = "forms"
	SourcePlace  Source = "place"
	SourceGuessr Source = "guessr"
	// SourceSelf is a signed-in person consenting for themselves through
	// core's API, from whichever app (the client is kept beside it).
	SourceSelf Source = "self"
)

// serviceSources are the sources a service account may speak for.
var serviceSources = []Source{SourceForms, SourcePlace, SourceGuessr}

// Status is where a grant is.
type Status string

const (
	// StatusPending waits for the person to confirm their address.
	StatusPending Status = "pending"
	// StatusActive is a confirmed grant: the person may be contacted.
	StatusActive Status = "active"
	// StatusWithdrawn and StatusExpired are ended grants.
	StatusWithdrawn Status = "withdrawn"
	StatusExpired   Status = "expired"
	// StatusSuperseded is a pending grant a product's verified grant for
	// the same address replaced: the newer row holds the proof.
	StatusSuperseded Status = "superseded"
)

// Ways a grant ends (contact_consents.ended_via).
const (
	viaLink              = "link"
	viaOneClick          = "one_click"
	viaSelf              = "self"
	viaService           = "service"
	viaRenewalUnanswered = "renewal_unanswered"
)

// WithdrawVia is how a person withdrew through a link.
type WithdrawVia string

const (
	// WithdrawByPage is the confirm page's button.
	WithdrawByPage WithdrawVia = viaLink
	// WithdrawByOneClick is a mail client's RFC 8058 one-click POST.
	WithdrawByOneClick WithdrawVia = viaOneClick
)

// Durations of the lifecycle (decisions S1-A and §7.2 of the retention
// proposal, docs/contact-consents.md).
const (
	// PendingTTL is how long a grant waits for its confirmation. The
	// confirmation link expires with it and the retention sweep deletes the
	// pending row after it.
	PendingTTL = 30 * 24 * time.Hour
	// RenewalAfter is the inactivity (no confirmation, renewal or attended
	// Event) after which a grant is due its renewal question.
	RenewalAfter = 3 * 365 * 24 * time.Hour
	// RenewalAnswerWindow is how long a renewal question waits for its
	// answer before the retention sweep ends the grant as expired.
	RenewalAnswerWindow = 60 * 24 * time.Hour
	// RenewalLinkTTL is how long a renewal link works: past the answer
	// window, so a late answer to a grant that is still open counts.
	RenewalLinkTTL = 90 * 24 * time.Hour
	// ProofRetention is how long an ended grant is kept as proof.
	ProofRetention = 3 * 365 * 24 * time.Hour
	// ConfirmationResend is the least time between two confirmation mails
	// of one pending grant, and MaxConfirmationMails how many it gets in
	// all: one when it is recorded, then at most one a day when the person
	// is named again, three in all. Whoever types an address cannot use core
	// to flood it.
	ConfirmationResend   = 24 * time.Hour
	MaxConfirmationMails = 3
	// lookupTimeout bounds one Lookup; the request's context carries no
	// deadline of its own.
	lookupTimeout = 10 * time.Second
	// grantAttempts is how often Grant tries when a concurrent grant for the
	// same subject wins the race to the open row.
	grantAttempts = 5
)

// Errors.
var (
	ErrInvalid     = errors.New("contact consent: invalid")
	ErrUnknownText = errors.New("contact consent: unknown text version")
	ErrLink        = errors.New("contact consent: invalid link")
	ErrLinkExpired = errors.New("contact consent: link expired")
	ErrNoSubject   = errors.New("contact consent: subject unknown")
	// ErrPurposeNotEnabled is a purpose core knows but does not take yet.
	ErrPurposeNotEnabled = errors.New("contact consent: purpose not enabled")
)

// KeyEnv holds the consent key: 32 random bytes, unpadded base64url, the
// format of ACCOUNT_DELETION_RECEIPT_KEY. Two keys are derived from it: one
// for the address HMAC kept as proof, one for the links. Changing it breaks
// every link already mailed and makes earlier proof rows unmatchable by
// address; do not rotate it.
const KeyEnv = "CONTACT_CONSENT_KEY"

// ServiceSourcesEnv maps service clients to the sources they speak for:
// source:client pairs separated by commas, "none" for none.
const ServiceSourcesEnv = "CONTACT_CONSENT_SERVICE_CLIENTS"

// DefaultServiceSources is used when ServiceSourcesEnv is unset.
const DefaultServiceSources = "forms:forms,place:place,guessr:guessr"

// ConfirmTemplateKeyEnv is the SkyMail template of the confirmation mail.
const ConfirmTemplateKeyEnv = "SKYMAIL_CONSENT_CONFIRM_TEMPLATE_KEY"

// DefaultConfirmTemplateKey is the confirmation mail's template when
// ConfirmTemplateKeyEnv is unset.
const DefaultConfirmTemplateKey = "core.contact-consent-confirm"

// VerifiedClientsEnv lists the service clients whose emailVerified core
// believes: comma-separated client ids, each one of ServiceSourcesEnv's.
// Unset or empty: none, so every product's grant waits for the person's
// confirmation (double opt-in).
const VerifiedClientsEnv = "CONTACT_CONSENT_VERIFIED_CLIENTS"

// TextURLEnv is the public address of the Açık Rıza Metni, required with
// KeyEnv: the confirm page links it. NoticeURLEnv is the KVKK aydınlatma
// metni, DefaultNoticeURL when unset.
const (
	TextURLEnv       = "CONTACT_CONSENT_TEXT_URL"
	NoticeURLEnv     = "CONTACT_CONSENT_NOTICE_URL"
	DefaultNoticeURL = "https://yildizskylab.com/kvkk-metni.pdf"
)

// Role names: client roles of core's Keycloak client
// (resource_access.core.roles), granted to service accounts only.
const (
	// RoleRecord lets a product's service account record and withdraw
	// grants for addresses its users typed (Forms, Place, Guessr).
	RoleRecord = "consent:record"
	// RoleAudienceRead lets SkyMail's service account read the invitation
	// audience and mark renewal questions as sent.
	RoleAudienceRead = "consent:audience:read"
)

// Config is the consent settings.
type Config struct {
	// Enabled is false when KeyEnv is unset: no grant is recorded and the
	// routes answer 503.
	Enabled bool
	key     []byte
	// LinkOrigin is core's public address, where the links point
	// (PUBLIC_API_ORIGIN).
	LinkOrigin string
	// ServiceSources maps a service client id to its source.
	ServiceSources map[string]Source
	// ConfirmTemplateKey is the SkyMail template of the confirmation mail.
	ConfirmTemplateKey string
	// VerifiedClients are the service clients whose emailVerified counts.
	VerifiedClients map[string]bool
	// TextURL and NoticeURL are the Açık Rıza Metni and the aydınlatma
	// metni the pages link.
	TextURL, NoticeURL string
}

var clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)

// ConfigFromEnv reads the consent settings. Without KeyEnv consents are off
// and nothing else is read. With it, PUBLIC_API_ORIGIN is required. Errors
// name variables, never values.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	raw := strings.TrimSpace(getenv(KeyEnv))
	if raw == "" {
		return Config{}, nil
	}
	key, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(key) != 32 {
		return Config{}, fmt.Errorf("%s must be an unpadded base64url-encoded 32-byte key", KeyEnv)
	}
	origin := strings.TrimRight(strings.TrimSpace(getenv("PUBLIC_API_ORIGIN")), "/")
	parsed, err := url.Parse(origin)
	if origin == "" || err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return Config{}, fmt.Errorf("%s needs PUBLIC_API_ORIGIN, core's public http(s) address, for its links", KeyEnv)
	}
	sources, err := parseServiceSources(getenv(ServiceSourcesEnv))
	if err != nil {
		return Config{}, err
	}
	template := DefaultConfirmTemplateKey
	if value, ok := lookup(getenv, ConfirmTemplateKeyEnv); ok {
		template = value
	}
	verified, err := parseVerifiedClients(getenv(VerifiedClientsEnv), sources)
	if err != nil {
		return Config{}, err
	}
	textURL, err := pageURL(getenv(TextURLEnv), "")
	if err != nil || textURL == "" {
		return Config{}, fmt.Errorf("%s needs %s, the public address of the Açık Rıza Metni the confirm page links", KeyEnv, TextURLEnv)
	}
	noticeURL, err := pageURL(getenv(NoticeURLEnv), DefaultNoticeURL)
	if err != nil {
		return Config{}, fmt.Errorf("%s must be an http(s) address", NoticeURLEnv)
	}
	return Config{
		Enabled: true, key: key, LinkOrigin: origin, ServiceSources: sources, ConfirmTemplateKey: template,
		VerifiedClients: verified, TextURL: textURL, NoticeURL: noticeURL,
	}, nil
}

// pageURL reads an absolute http(s) address, fallback when raw is empty.
func pageURL(raw, fallback string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", errors.New("not an http(s) address")
	}
	return raw, nil
}

func parseVerifiedClients(raw string, sources map[string]Source) (map[string]bool, error) {
	out := map[string]bool{}
	for _, entry := range strings.Split(raw, ",") {
		client := strings.TrimSpace(entry)
		if client == "" {
			continue
		}
		if _, ok := sources[client]; !ok {
			return nil, fmt.Errorf("%s: %q is not a client of %s", VerifiedClientsEnv, client, ServiceSourcesEnv)
		}
		out[client] = true
	}
	return out, nil
}

func lookup(getenv func(string) string, name string) (string, bool) {
	value := getenv(name)
	if value == "" {
		return "", false
	}
	return strings.TrimSpace(value), true
}

func parseServiceSources(raw string) (map[string]Source, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = DefaultServiceSources
	}
	out := map[string]Source{}
	if raw == "none" {
		return out, nil
	}
	seen := map[Source]bool{}
	for _, entry := range strings.Split(raw, ",") {
		source, client, ok := strings.Cut(strings.TrimSpace(entry), ":")
		if !ok || !clientIDPattern.MatchString(client) || !isServiceSource(Source(source)) {
			return nil, fmt.Errorf("%s: %q is not source:client with a source of %v", ServiceSourcesEnv, entry, serviceSources)
		}
		if seen[Source(source)] {
			return nil, fmt.Errorf("%s: %s named twice", ServiceSourcesEnv, source)
		}
		if _, taken := out[client]; taken {
			return nil, fmt.Errorf("%s: client %s named twice", ServiceSourcesEnv, client)
		}
		seen[Source(source)] = true
		out[client] = Source(source)
	}
	return out, nil
}

func isServiceSource(s Source) bool {
	for _, known := range serviceSources {
		if s == known {
			return true
		}
	}
	return false
}

// TestConfig is a Config for tests and tools: key must be 32 bytes. Only the
// clients in verified may assert a verified address.
func TestConfig(key []byte, origin string, verified ...string) Config {
	config := Config{
		Enabled: true, key: key, LinkOrigin: strings.TrimRight(origin, "/"),
		ServiceSources:     map[string]Source{"forms": SourceForms, "place": SourcePlace, "guessr": SourceGuessr},
		ConfirmTemplateKey: DefaultConfirmTemplateKey,
		VerifiedClients:    map[string]bool{},
		TextURL:            "https://example.test/acik-riza",
		NoticeURL:          DefaultNoticeURL,
	}
	for _, client := range verified {
		config.VerifiedClients[client] = true
	}
	return config
}

func (c Config) derive(label string) []byte {
	mac := hmac.New(sha256.New, c.key)
	mac.Write([]byte(label))
	return mac.Sum(nil)
}

// emailHMAC is the keyed digest of a normalized address: the proof key of a
// guest grant.
func (c Config) emailHMAC(email string) []byte {
	mac := hmac.New(sha256.New, c.derive("contact-consent email v1"))
	mac.Write([]byte(email))
	return mac.Sum(nil)
}

// NormalizeEmail is the form an address is kept and matched in: trimmed and
// lowercased, as Guest apply keeps it. It reports false for anything that is
// not shaped like one address.
func NormalizeEmail(raw string) (string, bool) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if len(email) < 3 || len(email) > 254 || strings.ContainsAny(email, " \t\r\n,;<>\"") {
		return "", false
	}
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "@") || !strings.Contains(domain, ".") {
		return "", false
	}
	return email, true
}

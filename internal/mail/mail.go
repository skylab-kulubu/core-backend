package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const DefaultAPIOrigin = "https://api.yildizskylab.com/api/skymail"

// Template keys SkyMail seeds for core. Both are system templates there: they
// cannot be archived and their key cannot be renamed, so a mail addressed by key
// survives a template being replaced, which a mail addressed by id does not.
const (
	DefaultWelcomeTemplateKey     = "core.welcome"
	DefaultCertificateTemplateKey = "core.certificate"
)

// Values carried by the diagnostic lines. They are fixed and low cardinality:
// nothing derived from a user, a template or a response body reaches the log.
const (
	eventCallFailed   = "skymail_call_failed"
	eventUnconfigured = "skymail_template_unconfigured"

	kindWelcome     = "welcome"
	kindCertificate = "certificate"
	kindToken       = "token"

	reasonTemplateMissing = "template_missing"
	reasonUpstreamError   = "upstream_error"
	reasonKeyFallback     = "template_key_missing_fallback_to_id"
	reasonUnconfigured    = "template_unconfigured"
)

const (
	// maxDiagnosticBody caps what is read from a refused response: enough for an
	// error envelope, small enough that a runaway body cannot be pulled into
	// memory on a path nobody is waiting on.
	maxDiagnosticBody = 2 << 10
	// maxDiagnosticCode caps the code taken out of that envelope. Anything
	// longer is prose rather than a code, and prose can carry a recipient.
	maxDiagnosticCode = 64
)

type Mailer interface {
	Welcome(ctx context.Context, u user.User)
}

func APIOrigin(raw string) string {
	if u := strings.TrimSpace(raw); u != "" {
		return strings.TrimRight(u, "/")
	}
	return DefaultAPIOrigin
}

type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

type StaticToken string

func (s StaticToken) Token(context.Context) (string, error) {
	return string(s), nil
}

type SkyMail struct {
	BaseURL                string
	TemplateID             uuid.UUID
	TemplateKey            string
	CertificateTemplateID  uuid.UUID
	CertificateTemplateKey string
	Tokens                 TokenSource
	HTTP                   *http.Client
	Logger                 *log.Logger
}

// template says which SkyMail template a send is aimed at. Only one of the two
// fields ever travels in a request body: SkyMail stops looking at the key as
// soon as an id is present, so sending both would silently pin the mail to the
// id and hide whichever addressing the deployment believed it was using.
type template struct {
	key string
	id  uuid.UUID
}

func (t template) configured() bool {
	return t.key != "" || t.id != uuid.Nil
}

// task renders one mail task body. The caller passes a template holding a single
// addressing, which is what keeps the two out of the same request.
func (t template) task(recipient, fullName string, vars map[string]string) map[string]any {
	body := map[string]any{
		"recipient_email":     recipient,
		"recipient_full_name": fullName,
		"body_variables":      vars,
	}
	if t.key != "" {
		body["template_key"] = t.key
		return body
	}
	body["template_id"] = t.id
	return body
}

func (s *SkyMail) welcomeTemplate() template {
	if s == nil {
		return template{}
	}
	return template{key: strings.TrimSpace(s.TemplateKey), id: s.TemplateID}
}

func (s *SkyMail) certificateTemplate() template {
	if s == nil {
		return template{}
	}
	return template{key: strings.TrimSpace(s.CertificateTemplateKey), id: s.CertificateTemplateID}
}

// Configured reports whether at least one mail kind can be addressed. A SkyMail
// that cannot address anything is not wired in as the mailer.
func (s *SkyMail) Configured() bool {
	return s.welcomeTemplate().configured() || s.certificateTemplate().configured()
}

// WarnUnconfiguredTemplates writes one line per mail kind that has neither a
// template key nor a template id. Startup is the only place that can say this
// once: the send path returns silently, as it always has, and would otherwise
// repeat the same line for every user who signs up.
func (s *SkyMail) WarnUnconfiguredTemplates() {
	if s == nil {
		return
	}
	for _, unconfigured := range []struct {
		kind string
		tpl  template
	}{
		{kind: kindWelcome, tpl: s.welcomeTemplate()},
		{kind: kindCertificate, tpl: s.certificateTemplate()},
	} {
		if !unconfigured.tpl.configured() {
			warn(s.Logger, eventUnconfigured, unconfigured.kind, 0, reasonUnconfigured, "")
		}
	}
}

func (s *SkyMail) Welcome(ctx context.Context, u user.User) {
	fullName := strings.TrimSpace(u.FirstName + " " + u.LastName)
	s.send(ctx, kindWelcome, s.welcomeTemplate(), u.Email, fullName, map[string]string{
		"FirstName": u.FirstName,
		"LastName":  u.LastName,
		"Email":     u.Email,
		"SkyNumber": u.SkyNumber,
		"CreatedAt": u.CreatedAt.UTC().Format(time.RFC3339),
	})
}

// send posts one mail task and tells its caller nothing: a mail SkyMail refuses
// must not fail the request that triggered it. The refusal goes to the log
// instead of being dropped, which is the whole point of this path.
func (s *SkyMail) send(ctx context.Context, kind string, tpl template, recipient, fullName string, vars map[string]string) {
	if s == nil || strings.TrimSpace(s.BaseURL) == "" || !tpl.configured() {
		return
	}
	token, err := s.Tokens.Token(ctx)
	if err != nil || token == "" {
		return
	}
	if tpl.key != "" {
		status := s.post(ctx, kind, token, template{key: tpl.key}.task(recipient, fullName, vars))
		// A key SkyMail does not know is the one refusal the id can still
		// answer, because the key may not be seeded yet. Every other status has
		// already been warned about and is not worth a second request.
		if status != http.StatusNotFound || tpl.id == uuid.Nil {
			return
		}
		warn(s.Logger, eventCallFailed, kind, status, reasonKeyFallback, "")
	}
	if tpl.id == uuid.Nil {
		return
	}
	s.post(ctx, kind, token, template{id: tpl.id}.task(recipient, fullName, vars))
}

// post sends one mail task and returns the status SkyMail answered with, or 0
// when the request never reached it. A refusal is warned about here so that
// every send shares one rule.
func (s *SkyMail) post(ctx context.Context, kind, token string, task map[string]any) int {
	body, err := json.Marshal(task)
	if err != nil {
		return 0
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.BaseURL, "/")+"/v1/mail_tasks/single", bytes.NewReader(body))
	if err != nil {
		return 0
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := s.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if succeeded(resp.StatusCode) {
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	warn(s.Logger, eventCallFailed, kind, resp.StatusCode, sendReason(resp.StatusCode), diagnosticCode(diagnosticBody(resp.Body)))
	return resp.StatusCode
}

// succeeded holds the only definition of a delivered call: SkyMail answers 201
// to a single send, 200 or 204 to the list calls, and anything outside 2xx means
// no mail task row was written.
func succeeded(status int) bool {
	return status >= 200 && status <= 299
}

// sendReason classifies a refused send no further than the status allows. A 404
// is the archived or unknown template the owner has to fix; everything else is
// only known to have come back wrong, and the raw status travels with it.
func sendReason(status int) string {
	if status == http.StatusNotFound {
		return reasonTemplateMissing
	}
	return reasonUpstreamError
}

// warn writes the one line every SkyMail diagnostic shares. It carries fixed
// values plus the remote status: never the recipient, the template, the token or
// the response body, so the log stays safe to keep and to forward.
func warn(logger *log.Logger, event, kind string, status int, reason, code string) {
	if logger == nil {
		logger = log.Default()
	}
	payload, err := json.Marshal(struct {
		Event  string `json:"event"`
		Level  string `json:"level"`
		Kind   string `json:"kind"`
		Status int    `json:"status,omitempty"`
		Reason string `json:"reason"`
		Code   string `json:"code,omitempty"`
	}{
		Event:  event,
		Level:  "warn",
		Kind:   kind,
		Status: status,
		Reason: reason,
		Code:   code,
	})
	if err == nil {
		logger.Print(string(payload))
	}
}

// diagnosticBody reads the front of a refused response and leaves the rest to
// Close. Nobody is waiting on this path, so a large body is not worth draining.
func diagnosticBody(body io.Reader) []byte {
	raw, err := io.ReadAll(io.LimitReader(body, maxDiagnosticBody))
	if err != nil {
		return nil
	}
	return raw
}

// diagnosticCode pulls a short code out of a small JSON error envelope. A body
// that is oversized, truncated, not JSON or not code-shaped yields nothing,
// which is the safe answer: the line still carries the status.
func diagnosticCode(body []byte) string {
	if len(body) == 0 || len(body) > maxDiagnosticBody {
		return ""
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return ""
	}
	for _, name := range []string{"error", "message"} {
		var value string
		if json.Unmarshal(fields[name], &value) == nil && codeShaped(value) {
			return value
		}
	}
	return ""
}

// codeShaped reports whether value is a bare code. Prose, mail addresses and
// identifiers are rejected, so a remote body cannot smuggle a recipient or a
// template id into the log through the field core is willing to print.
func codeShaped(value string) bool {
	if value == "" || len(value) > maxDiagnosticCode {
		return false
	}
	if _, err := uuid.Parse(value); err == nil {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

type ClientCredentials struct {
	TokenURL     string
	ClientID     string
	ClientSecret string
	HTTP         *http.Client
	Logger       *log.Logger
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
}

func (c ClientCredentials) Token(ctx context.Context) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", c.ClientID)
	form.Set("client_secret", c.ClientSecret)
	form.Set("scope", "openid")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	// The senders drop this error to stay fire-and-forget, so a refused token is
	// the second way a mail can disappear without a trace. It is warned about
	// here, where the status still exists.
	if !succeeded(resp.StatusCode) {
		warn(c.Logger, eventCallFailed, kindToken, resp.StatusCode, reasonUpstreamError, diagnosticCode(diagnosticBody(resp.Body)))
		return "", fmt.Errorf("token status %d", resp.StatusCode)
	}
	var out tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.AccessToken, nil
}

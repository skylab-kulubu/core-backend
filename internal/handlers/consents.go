package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"slices"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/consent"
)

// ConsentHandler serves contact consents (docs/contact-consents.md): the
// public confirm and withdraw pages, a person's own grants, the products'
// service routes, and the consents field of Guest apply.
type ConsentHandler struct {
	svc *consent.Service
}

// NewConsentHandler serves svc. A nil or disabled service answers every
// route 503.
func NewConsentHandler(svc *consent.Service) *ConsentHandler {
	return &ConsentHandler{svc: svc}
}

const (
	maxLookupAddresses = 500
	maxRenewalIDs      = consent.MaxAudiencePage
)

func consentError(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, consent.ErrDisabled):
		c.Set(fiber.HeaderCacheControl, "no-store")
		return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", "consents_unavailable")
	case errors.Is(err, consent.ErrUnknownText):
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "consent_text_unknown")
	case errors.Is(err, consent.ErrInvalid):
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "consent_invalid")
	}
	log.Printf("contact consent: %v", err)
	return problem(c, fiber.StatusInternalServerError, "Internal Server Error")
}

func (h *ConsentHandler) enabled() bool { return h.svc.Enabled() }

// errAnswered means the refusal is already written to the response.
var errAnswered = errors.New("answered")

// answered turns errAnswered back into the handler's nil.
func answered(err error) error {
	if errors.Is(err, errAnswered) {
		return nil
	}
	return err
}

// service is the calling service account when it holds role; for
// consent.RoleRecord also the source its client speaks for. A refusal is
// written to the response and reported as errAnswered.
func (h *ConsentHandler) service(c fiber.Ctx, role string) (authn.Identity, consent.Source, error) {
	ident, ok := c.Locals(authn.LocalsIdentity).(authn.Identity)
	if !ok {
		return authn.Identity{}, "", refuse(problem(c, fiber.StatusUnauthorized, "Unauthorized"))
	}
	if !ident.ServiceAccount || !slices.Contains(ident.Roles, role) {
		return authn.Identity{}, "", refuse(problemCode(c, fiber.StatusForbidden, "Forbidden", "consent_role_missing"))
	}
	if role != consent.RoleRecord {
		return ident, "", nil
	}
	source, ok := h.svc.ServiceSource(ident.Client)
	if !ok {
		return authn.Identity{}, "", refuse(problemCode(c, fiber.StatusForbidden, "Forbidden", "consent_source_unknown"))
	}
	return ident, source, nil
}

func refuse(err error) error {
	if err != nil {
		return err
	}
	return errAnswered
}

// person is the signed-in person (never a service account).
func person(c fiber.Ctx) (authn.Identity, error) {
	ident, ok := c.Locals(authn.LocalsIdentity).(authn.Identity)
	if !ok {
		return authn.Identity{}, refuse(problem(c, fiber.StatusUnauthorized, "Unauthorized"))
	}
	if ident.ServiceAccount {
		return authn.Identity{}, refuse(problemCode(c, fiber.StatusForbidden, "Forbidden", "consent_person_only"))
	}
	return ident, nil
}

type grantBody struct {
	Purpose       string `json:"purpose"`
	TextVersion   string `json:"textVersion"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"emailVerified"`
	Name          string `json:"name"`
}

type grantAnswer struct {
	Status consent.Status `json:"status"`
}

// Record is POST /v1/consents: a product's service account records the
// consent a person gave in the product, for the address they typed.
func (h *ConsentHandler) Record(c fiber.Ctx) error {
	if !h.enabled() {
		return consentError(c, consent.ErrDisabled)
	}
	ident, source, err := h.service(c, consent.RoleRecord)
	if err != nil {
		return answered(err)
	}
	var body grantBody
	if err := c.Bind().Body(&body); err != nil {
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "consent_invalid")
	}
	purpose, ok := consent.ParsePurpose(body.Purpose)
	if !ok {
		return consentError(c, consent.ErrInvalid)
	}
	result, err := h.svc.Grant(c.Context(), consent.Grant{
		Purpose: purpose, TextVersion: strings.TrimSpace(body.TextVersion), Email: body.Email, Source: source,
		ClientID: ident.Client, EmailVerified: body.EmailVerified, Name: body.Name,
	})
	if err != nil {
		return consentError(c, err)
	}
	status := fiber.StatusOK
	if result.Created {
		status = fiber.StatusCreated
	}
	return c.Status(status).JSON(grantAnswer{Status: result.Status})
}

type addressBody struct {
	Purpose string `json:"purpose"`
	Email   string `json:"email"`
}

// WithdrawForAddress is POST /v1/consents/withdrawals: the person unticked
// the box in the product. The address travels in the body, not the path.
func (h *ConsentHandler) WithdrawForAddress(c fiber.Ctx) error {
	if !h.enabled() {
		return consentError(c, consent.ErrDisabled)
	}
	if _, _, err := h.service(c, consent.RoleRecord); err != nil {
		return answered(err)
	}
	var body addressBody
	if err := c.Bind().Body(&body); err != nil {
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "consent_invalid")
	}
	purpose, ok := consent.ParsePurpose(body.Purpose)
	if !ok {
		return consentError(c, consent.ErrInvalid)
	}
	withdrawn, err := h.svc.WithdrawForAddress(c.Context(), purpose, body.Email)
	if err != nil {
		return consentError(c, err)
	}
	return c.JSON(fiber.Map{"withdrawn": withdrawn})
}

type consentLookupBody struct {
	Purpose string   `json:"purpose"`
	Emails  []string `json:"emails"`
}

// Lookup is POST /v1/consents/lookup: the state of the addresses' open
// grants for a purpose. An address with none open is absent.
func (h *ConsentHandler) Lookup(c fiber.Ctx) error {
	if !h.enabled() {
		return consentError(c, consent.ErrDisabled)
	}
	if _, _, err := h.service(c, consent.RoleRecord); err != nil {
		return answered(err)
	}
	var body consentLookupBody
	if err := c.Bind().Body(&body); err != nil || len(body.Emails) > maxLookupAddresses {
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "consent_invalid")
	}
	purpose, ok := consent.ParsePurpose(body.Purpose)
	if !ok {
		return consentError(c, consent.ErrInvalid)
	}
	states, err := h.svc.Lookup(c.Context(), purpose, body.Emails)
	if err != nil {
		return consentError(c, err)
	}
	return c.JSON(fiber.Map{"states": states})
}

// Audience is GET /v1/consents/audience: SkyMail's list of the addresses a
// purpose's mail may go to.
func (h *ConsentHandler) Audience(c fiber.Ctx) error {
	if !h.enabled() {
		return consentError(c, consent.ErrDisabled)
	}
	if _, _, err := h.service(c, consent.RoleAudienceRead); err != nil {
		return answered(err)
	}
	purpose, ok := consent.ParsePurpose(c.Query("purpose"))
	if !ok {
		return consentError(c, consent.ErrInvalid)
	}
	after := uuid.Nil
	if raw := c.Query("after"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			return consentError(c, consent.ErrInvalid)
		}
		after = parsed
	}
	limit := consent.MaxAudiencePage
	if raw := c.Query("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > consent.MaxAudiencePage {
			return consentError(c, consent.ErrInvalid)
		}
		limit = parsed
	}
	entries, next, err := h.svc.Audience(c.Context(), purpose, after, limit)
	if err != nil {
		return consentError(c, err)
	}
	if entries == nil {
		entries = []consent.AudienceEntry{}
	}
	answer := fiber.Map{"items": entries, "next": nil}
	if next != uuid.Nil {
		answer["next"] = next.String()
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(answer)
}

type renewalBody struct {
	Purpose string      `json:"purpose"`
	IDs     []uuid.UUID `json:"ids"`
}

// RequestRenewal is POST /v1/consents/renewal-requests: SkyMail sent the
// renewal question to these grants.
func (h *ConsentHandler) RequestRenewal(c fiber.Ctx) error {
	if !h.enabled() {
		return consentError(c, consent.ErrDisabled)
	}
	if _, _, err := h.service(c, consent.RoleAudienceRead); err != nil {
		return answered(err)
	}
	var body renewalBody
	if err := c.Bind().Body(&body); err != nil || len(body.IDs) == 0 || len(body.IDs) > maxRenewalIDs {
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "consent_invalid")
	}
	purpose, ok := consent.ParsePurpose(body.Purpose)
	if !ok {
		return consentError(c, consent.ErrInvalid)
	}
	n, err := h.svc.RequestRenewal(c.Context(), purpose, body.IDs)
	if err != nil {
		return consentError(c, err)
	}
	return c.JSON(fiber.Map{"requested": n})
}

type ownGrantBody struct {
	Purpose     string `json:"purpose"`
	TextVersion string `json:"textVersion"`
}

// GrantMine is POST /v1/users/me/consents: a signed-in person consents for
// their own account (Account Center, Place or Guessr with their sign-in).
func (h *ConsentHandler) GrantMine(c fiber.Ctx) error {
	if !h.enabled() {
		return consentError(c, consent.ErrDisabled)
	}
	ident, err := person(c)
	if err != nil {
		return answered(err)
	}
	var body ownGrantBody
	if err := c.Bind().Body(&body); err != nil {
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "consent_invalid")
	}
	purpose, ok := consent.ParsePurpose(body.Purpose)
	if !ok {
		return consentError(c, consent.ErrInvalid)
	}
	result, err := h.svc.Grant(c.Context(), consent.Grant{
		Purpose: purpose, TextVersion: strings.TrimSpace(body.TextVersion), UserID: ident.ID,
		Source: consent.SourceSelf, ClientID: ident.Client,
	})
	if err != nil {
		return consentError(c, err)
	}
	status := fiber.StatusOK
	if result.Created {
		status = fiber.StatusCreated
	}
	return c.Status(status).JSON(grantAnswer{Status: result.Status})
}

// Mine is GET /v1/users/me/consents.
func (h *ConsentHandler) Mine(c fiber.Ctx) error {
	if !h.enabled() {
		return consentError(c, consent.ErrDisabled)
	}
	ident, err := person(c)
	if err != nil {
		return answered(err)
	}
	own, err := h.svc.Mine(c.Context(), ident.ID)
	if err != nil {
		return consentError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(fiber.Map{"items": own})
}

// WithdrawMine is DELETE /v1/users/me/consents/:purpose.
func (h *ConsentHandler) WithdrawMine(c fiber.Ctx) error {
	if !h.enabled() {
		return consentError(c, consent.ErrDisabled)
	}
	ident, err := person(c)
	if err != nil {
		return answered(err)
	}
	purpose, ok := consent.ParsePurpose(c.Params("purpose"))
	if !ok {
		return consentError(c, consent.ErrInvalid)
	}
	n, err := h.svc.WithdrawMine(c.Context(), ident.ID, purpose)
	if err != nil {
		return consentError(c, err)
	}
	return c.JSON(fiber.Map{"withdrawn": n})
}

// guestConsent is one entry of Guest apply's consents: a purpose name, or
// an object naming the text version the person was shown.
type guestConsent struct {
	Purpose     string `json:"purpose"`
	TextVersion string `json:"textVersion"`
}

func (g *guestConsent) UnmarshalJSON(raw []byte) error {
	var name string
	if err := json.Unmarshal(raw, &name); err == nil {
		g.Purpose = name
		return nil
	}
	type plain guestConsent
	return json.Unmarshal(raw, (*plain)(g))
}

type guestConsentBody struct {
	FirstName string         `json:"firstName"`
	LastName  string         `json:"lastName"`
	Email     string         `json:"email"`
	Consents  []guestConsent `json:"consents"`
}

// GuestApplyConsents is the consents field of Guest apply
// (docs/guest-apply.md). It runs before the handler: a field it cannot read
// is 400 and no Ticket is written. Absent or empty, nothing is recorded: the
// box is unticked unless the person ticked it. After a 201 it records each
// grant for the guest's address. Every such grant waits for the person's
// confirmation from the mail core sends (double opt-in), whoever called:
// the route is public, and a staff member adding a guest cannot consent for
// them. A grant that cannot be recorded turns the answer into 503, so the
// caller sends the application again (it finds its Ticket) instead of
// believing the consent is kept.
func (h *ConsentHandler) GuestApplyConsents(c fiber.Ctx) error {
	var body guestConsentBody
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		if bytes.Contains(c.Body(), []byte(`"consents"`)) {
			return problemCode(c, fiber.StatusBadRequest, "Bad Request", "consent_invalid")
		}
		// A body that is not this middleware's to judge: the handler
		// answers it.
		return c.Next()
	}
	if len(body.Consents) == 0 {
		return c.Next()
	}
	source, client := consent.SourceGuestApply, ""
	if ident, ok := c.Locals(authn.LocalsIdentity).(authn.Identity); ok && ident.ServiceAccount && h.svc != nil {
		if mapped, ok := h.svc.ServiceSource(ident.Client); ok {
			source, client = mapped, ident.Client
		}
	}
	grants := make([]consent.Grant, 0, len(body.Consents))
	for _, entry := range body.Consents {
		purpose, ok := consent.ParsePurpose(entry.Purpose)
		if !ok || !purpose.Allows(source) || !purpose.Allows(consent.SourceGuestApply) {
			return problemCode(c, fiber.StatusBadRequest, "Bad Request", "consent_invalid")
		}
		version := strings.TrimSpace(entry.TextVersion)
		if version == "" {
			version, _ = consent.CurrentText(purpose)
		}
		if !consent.KnownText(purpose, version) {
			return problemCode(c, fiber.StatusBadRequest, "Bad Request", "consent_text_unknown")
		}
		grants = append(grants, consent.Grant{
			Purpose: purpose, TextVersion: version, Email: body.Email, Source: source, ClientID: client,
			Name: strings.TrimSpace(body.FirstName + " " + body.LastName),
		})
	}
	if err := c.Next(); err != nil || c.Response().StatusCode() != fiber.StatusCreated {
		return err
	}
	if !h.enabled() {
		log.Printf("contact consent: %d guest apply grant(s) not recorded: %s is not set", len(grants), consent.KeyEnv)
		return nil
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return nil
	}
	for _, grant := range grants {
		grant.EventID = &eventID
		if _, err := h.svc.Grant(c.Context(), grant); err != nil {
			log.Printf("contact consent: guest apply grant not recorded: %v", err)
			c.Set(fiber.HeaderRetryAfter, "1")
			return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", "consent_not_recorded")
		}
	}
	return nil
}

// The public pages: no sign-in, the signed token is the permission.

// tokenFrom reads the link token of a POST: the query (an RFC 8058
// one-click POST goes to the List-Unsubscribe address as it is), else the
// form.
func tokenFrom(c fiber.Ctx) string {
	if token := strings.TrimSpace(c.Query("token")); token != "" {
		return token
	}
	return strings.TrimSpace(c.FormValue("token"))
}

// WithdrawPage is GET /v1/consents/withdraw: it asks before it acts, so a
// mail scanner that opens the link withdraws nothing.
func (h *ConsentHandler) WithdrawPage(c fiber.Ctx) error {
	token := strings.TrimSpace(c.Query("token"))
	if err := h.svc.CheckWithdrawLink(token); err != nil {
		return linkErrorPage(c, err)
	}
	return renderPage(c, fiber.StatusOK, page{
		Title: "Onayını geri al",
		Lines: []string{
			"Bu bağlantı SKY LAB'a verdiğin bir onaya ait: gelecek etkinliklere davet e-postaları ya da başvurunun sonraki alımlarda değerlendirilmek üzere saklanması.",
			"Geri alırsan bu amaçla sana e-posta gönderilmez ve verin bu amaçla saklanmaz. İstediğin zaman yeniden onay verebilirsin.",
		},
		Action: consent.WithdrawPath, Token: token, Button: "Onayımı geri al",
	})
}

// Withdraw is POST /v1/consents/withdraw: the page's button, or a mail
// client's one-click unsubscribe (RFC 8058: the body is
// List-Unsubscribe=One-Click and the token is in the address). It can be
// repeated: a second POST answers as the first.
func (h *ConsentHandler) Withdraw(c fiber.Ctx) error {
	oneClick := c.FormValue("List-Unsubscribe") == "One-Click"
	via := consent.WithdrawByPage
	if oneClick {
		via = consent.WithdrawByOneClick
	}
	outcome, err := h.svc.Withdraw(c.Context(), tokenFrom(c), via)
	if oneClick {
		// RFC 8058 §3.2: no redirect, no page to act on.
		c.Set(fiber.HeaderCacheControl, "no-store")
		switch {
		case err == nil:
			return c.Status(fiber.StatusOK).SendString("ok")
		case errors.Is(err, consent.ErrDisabled):
			return c.Status(fiber.StatusServiceUnavailable).SendString("unavailable")
		case errors.Is(err, consent.ErrLink):
			return c.Status(fiber.StatusBadRequest).SendString("invalid link")
		}
		log.Printf("contact consent: one-click withdraw: %v", err)
		return c.Status(fiber.StatusInternalServerError).SendString("error")
	}
	if err != nil {
		return linkErrorPage(c, err)
	}
	if outcome == consent.OutcomeNothingOpen {
		return renderPage(c, fiber.StatusOK, page{
			Title: "Onay zaten yok",
			Lines: []string{"Bu onay zaten geri alınmış ya da sona ermiş. Yapman gereken bir şey yok."},
		})
	}
	return renderPage(c, fiber.StatusOK, page{
		Title: "Onayın geri alındı",
		Lines: []string{"Artık bu amaçla sana e-posta gönderilmeyecek. Fikrini değiştirirsen bir sonraki kayıtta kutuyu yeniden işaretleyebilirsin."},
	})
}

// ConfirmPage is GET /v1/consents/confirm.
func (h *ConsentHandler) ConfirmPage(c fiber.Ctx) error {
	token := strings.TrimSpace(c.Query("token"))
	if err := h.svc.CheckConfirmLink(token); err != nil {
		return linkErrorPage(c, err)
	}
	return renderPage(c, fiber.StatusOK, page{
		Title: "E-posta adresini onayla",
		Lines: []string{
			"Bu adresle SKY LAB'a bir onay verildi: gelecek etkinliklere davet e-postaları ya da başvurunun sonraki alımlarda değerlendirilmek üzere saklanması.",
			"Onayı sen verdiysen aşağıdaki düğmeyle onayla. Sen vermediysen bu sayfayı kapat; onaylanmayan kayıt 30 gün içinde kendiliğinden silinir.",
		},
		Action: consent.ConfirmPath, Token: token, Button: "Onaylıyorum",
	})
}

// Confirm is POST /v1/consents/confirm.
func (h *ConsentHandler) Confirm(c fiber.Ctx) error {
	outcome, err := h.svc.Confirm(c.Context(), tokenFrom(c))
	if err != nil {
		return linkErrorPage(c, err)
	}
	switch outcome {
	case consent.OutcomeEnded:
		return renderPage(c, fiber.StatusOK, page{
			Title: "Bu onay geri alınmış",
			Lines: []string{"Bu onay daha önce geri alındı ya da sona erdi. Yeniden vermek istersen bir sonraki kayıtta kutuyu işaretleyebilirsin."},
		})
	case consent.OutcomeRenewed:
		return renderPage(c, fiber.StatusOK, page{
			Title: "Onayın yenilendi",
			Lines: []string{"Teşekkürler. Her e-postadaki bağlantıyla istediğin zaman geri alabilirsin."},
		})
	}
	return renderPage(c, fiber.StatusOK, page{
		Title: "Onayın kaydedildi",
		Lines: []string{"Teşekkürler. Her e-postadaki bağlantıyla istediğin zaman geri alabilirsin."},
	})
}

func linkErrorPage(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, consent.ErrDisabled):
		return renderPage(c, fiber.StatusServiceUnavailable, page{
			Title: "Şu an yapılamıyor", Lines: []string{"Bu işlem şu an kullanılamıyor. Lütfen daha sonra yeniden dene."},
		})
	case errors.Is(err, consent.ErrLinkExpired), errors.Is(err, consent.ErrNoSubject):
		return renderPage(c, fiber.StatusGone, page{
			Title: "Bağlantının süresi dolmuş",
			Lines: []string{"Bu bağlantı artık geçerli değil. Onay vermek istersen bir sonraki kayıtta kutuyu yeniden işaretleyebilirsin."},
		})
	case errors.Is(err, consent.ErrLink), errors.Is(err, consent.ErrInvalid):
		return renderPage(c, fiber.StatusBadRequest, page{
			Title: "Bağlantı geçersiz", Lines: []string{"Bu bağlantı geçersiz. E-postadaki bağlantıyı eksiksiz açtığından emin ol."},
		})
	}
	log.Printf("contact consent link: %v", err)
	return renderPage(c, fiber.StatusInternalServerError, page{
		Title: "Bir sorun oldu", Lines: []string{"İşlem tamamlanamadı. Lütfen biraz sonra yeniden dene."},
	})
}

type page struct {
	Title  string
	Lines  []string
	Action string
	Token  string
	Button string
}

var pageTemplate = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="tr">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>{{.Title}} · SKY LAB</title>
<style>
body{margin:0;font-family:system-ui,-apple-system,"Segoe UI",sans-serif;background:#f4f5f7;color:#1d2433}
main{max-width:32rem;margin:12vh auto;padding:2rem;background:#fff;border-radius:12px;box-shadow:0 1px 4px rgba(0,0,0,.08)}
h1{font-size:1.4rem;margin:0 0 1rem}p{line-height:1.5}
button{font:inherit;padding:.7rem 1.4rem;border:0;border-radius:8px;background:#1d4ed8;color:#fff;cursor:pointer}
@media (prefers-color-scheme:dark){body{background:#11151c;color:#e6e9ef}main{background:#1b212b}}
</style>
</head>
<body>
<main>
<h1>{{.Title}}</h1>
{{range .Lines}}<p>{{.}}</p>
{{end}}{{if .Action}}<form method="post" action="{{.Action}}">
<input type="hidden" name="token" value="{{.Token}}">
<button type="submit">{{.Button}}</button>
</form>
{{end}}<p><small>SKY LAB · Yıldız Teknik Üniversitesi</small></p>
</main>
</body>
</html>
`))

func renderPage(c fiber.Ctx, status int, p page) error {
	var out bytes.Buffer
	if err := pageTemplate.Execute(&out, p); err != nil {
		return err
	}
	// The address carries the token: no referrer, no cache, no framing.
	c.Set(fiber.HeaderCacheControl, "no-store")
	c.Set("Referrer-Policy", "no-referrer")
	c.Set(fiber.HeaderXFrameOptions, "DENY")
	c.Set("X-Robots-Tag", "noindex")
	c.Set(fiber.HeaderContentSecurityPolicy, "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
	return c.Status(status).Send(out.Bytes())
}

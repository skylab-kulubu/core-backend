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
	case errors.Is(err, consent.ErrAddressesUnavailable):
		// Keycloak unreachable: a passing failure, not core's fault. The
		// error carries no address; it says what failed.
		log.Printf("contact consent: %v", err)
		c.Set(fiber.HeaderCacheControl, "no-store")
		c.Set(fiber.HeaderRetryAfter, "30")
		return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", "consent_addresses_unavailable")
	case errors.Is(err, consent.ErrUnknownText):
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "consent_text_unknown")
	case errors.Is(err, consent.ErrPurposeNotEnabled):
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "purpose_not_enabled")
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
}

type grantAnswer struct {
	Status consent.Status `json:"status"`
}

// Record is POST /v1/consents: a product's service account records the
// consent a person gave in the product, for the address they typed. Its
// emailVerified counts only for a client of CONTACT_CONSENT_VERIFIED_CLIENTS;
// any other grant answers pending and waits for the person's confirmation.
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
	purpose, err := consent.ParsePurpose(body.Purpose)
	if err != nil {
		return consentError(c, err)
	}
	result, err := h.svc.Grant(c.Context(), consent.Grant{
		Purpose: purpose, TextVersion: strings.TrimSpace(body.TextVersion), Email: body.Email, Source: source,
		ClientID: ident.Client, EmailVerified: body.EmailVerified,
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
	purpose, err := consent.ParsePurpose(body.Purpose)
	if err != nil {
		return consentError(c, err)
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
	purpose, err := consent.ParsePurpose(body.Purpose)
	if err != nil {
		return consentError(c, err)
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
	purpose, err := consent.ParsePurpose(c.Query("purpose"))
	if err != nil {
		return consentError(c, err)
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
	purpose, err := consent.ParsePurpose(body.Purpose)
	if err != nil {
		return consentError(c, err)
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
	purpose, err := consent.ParsePurpose(body.Purpose)
	if err != nil {
		return consentError(c, err)
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
	purpose, err := consent.ParsePurpose(c.Params("purpose"))
	if err != nil {
		return consentError(c, err)
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
	Email    string         `json:"email"`
	Consents []guestConsent `json:"consents"`
}

// GuestApplyConsents is the consents field of Guest apply
// (docs/guest-apply.md). It runs before the handler and judges the whole
// field there, the guest's address included: a field it cannot record is
// 400 and no Ticket is written. Absent or empty, nothing is recorded: the box
// is unticked unless the person ticked it. It names each enabled purpose at
// most once (a repeated purpose with the same text counts once). After a 201
// it records each grant for the guest's address. Every such grant waits for
// the person's confirmation from the mail core sends (double opt-in),
// whoever called: the route is public, and a staff member adding a guest
// cannot consent for them. A grant that cannot be recorded for a passing
// reason (the database) turns the answer into 503, so the caller sends the
// application again (it finds its Ticket) instead of believing the consent
// is kept.
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
	if len(body.Consents) > consent.EnabledPurposes() {
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "consent_invalid")
	}
	source, client := consent.SourceGuestApply, ""
	if ident, ok := c.Locals(authn.LocalsIdentity).(authn.Identity); ok && ident.ServiceAccount && h.svc != nil {
		if mapped, ok := h.svc.ServiceSource(ident.Client); ok {
			source, client = mapped, ident.Client
		}
	}
	grants := make([]consent.Grant, 0, len(body.Consents))
	named := map[consent.Purpose]string{}
	for _, entry := range body.Consents {
		purpose, err := consent.ParsePurpose(entry.Purpose)
		if err != nil {
			return consentError(c, err)
		}
		if !purpose.Allows(source) || !purpose.Allows(consent.SourceGuestApply) {
			return problemCode(c, fiber.StatusBadRequest, "Bad Request", "consent_invalid")
		}
		grant := consent.Grant{
			Purpose: purpose, TextVersion: strings.TrimSpace(entry.TextVersion), Email: body.Email, Source: source, ClientID: client,
		}
		// The same check Grant makes, the address included, before
		// anything is written.
		if err := grant.Validate(); err != nil {
			return consentError(c, err)
		}
		if version, seen := named[purpose]; seen {
			if version != grant.TextVersion {
				return problemCode(c, fiber.StatusBadRequest, "Bad Request", "consent_invalid")
			}
			continue
		}
		named[purpose] = grant.TextVersion
		grants = append(grants, grant)
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
		_, err := h.svc.Grant(c.Context(), grant)
		switch {
		case err == nil:
		case errors.Is(err, consent.ErrInvalid), errors.Is(err, consent.ErrUnknownText), errors.Is(err, consent.ErrPurposeNotEnabled):
			// Judged above; a retry would meet the same answer, so the
			// Ticket's answer stands.
			log.Printf("contact consent: guest apply grant refused after the Ticket: %v", err)
		default:
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

// WithdrawPage is GET /v1/consents/withdraw: it names the consent the link
// is for and asks before it acts, so a mail scanner that opens the link
// withdraws nothing.
func (h *ConsentHandler) WithdrawPage(c fiber.Ctx) error {
	token := strings.TrimSpace(c.Query("token"))
	grant, err := h.svc.WithdrawLinkGrant(c.Context(), token)
	if errors.Is(err, consent.ErrNoSubject) {
		return renderPage(c, fiber.StatusOK, page{
			Title: "Onay zaten yok",
			Lines: []string{"Bu bağlantının ait olduğu onay artık kayıtlı değil. Yapman gereken bir şey yok."},
		})
	}
	if err != nil {
		return linkErrorPage(c, err)
	}
	return renderPage(c, fiber.StatusOK, page{
		Title: "Onayını geri al",
		Lines: []string{
			"Bu bağlantı şu onaya ait: " + grant.Purpose.Label() + ".",
			"Geri alırsan bu amaçla sana e-posta gönderilmez. İstediğin zaman yeniden onay verebilirsin.",
		},
		Links:  h.textLinks(),
		Action: consent.WithdrawPath, Token: token, Button: "Onayımı geri al",
	})
}

// Withdraw is POST /v1/consents/withdraw: the page's button, or a mail
// client's one-click unsubscribe (RFC 8058: the body is
// List-Unsubscribe=One-Click, form-encoded or multipart, and the token is in
// the address). It can be repeated: a second POST answers as the first.
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

// ConfirmPage is GET /v1/consents/confirm. It says exactly what the person
// confirms: the purpose of the grant the link names and the text it was
// given on, with the Açık Rıza Metni and the aydınlatma metni. Opening it
// changes nothing.
func (h *ConsentHandler) ConfirmPage(c fiber.Ctx) error {
	token := strings.TrimSpace(c.Query("token"))
	grant, err := h.svc.ConfirmLinkGrant(c.Context(), token)
	if err != nil {
		return linkErrorPage(c, err)
	}
	text := "“" + consent.Text(grant.TextVersion) + "”"
	switch grant.Status {
	case consent.StatusPending:
		return renderPage(c, fiber.StatusOK, page{
			Title: "E-posta adresinle verilen onayı doğrula",
			Lines: []string{
				"Bu e-posta adresiyle SKY LAB'a yalnız şu onay verildi: " + grant.Purpose.Label() + ".",
				"Kutunun yanındaki metin:",
			},
			Quote: text,
			After: []string{
				"Onayı sen verdiysen aşağıdaki düğmeyle doğrula. Sen vermediysen bu sayfayı kapatman yeter: doğrulanmayan onayla sana bu amaçla e-posta gönderilmez.",
			},
			Links:  h.textLinks(),
			Action: consent.ConfirmPath, Token: token, Button: "Onaylıyorum",
		})
	case consent.StatusActive:
		return renderPage(c, fiber.StatusOK, page{
			Title: "Onayın sürsün mü?",
			Lines: []string{
				"Bu bağlantı şu onaya ait: " + grant.Purpose.Label() + ".",
				"Kutunun yanındaki metin:",
			},
			Quote:  text,
			After:  []string{"Sürmesini istiyorsan aşağıdaki düğmeye bas. İstemiyorsan bir şey yapma ya da e-postadaki geri alma bağlantısını kullan."},
			Links:  h.textLinks(),
			Action: consent.ConfirmPath, Token: token, Button: "Sürsün",
		})
	case consent.StatusSuperseded:
		return renderPage(c, fiber.StatusOK, page{Title: "Onayın zaten kayıtlı", Lines: []string{supersededLine}})
	}
	return renderPage(c, fiber.StatusOK, page{Title: "Bu onay geri alınmış", Lines: []string{endedLine}})
}

const (
	supersededLine = "Bu adres için onay, adresini doğrulayan bir SKY LAB uygulamasından kayda geçti. Yapman gereken bir şey yok."
	endedLine      = "Bu onay daha önce geri alındı ya da sona erdi. Yeniden vermek istersen bir sonraki kayıtta kutuyu işaretleyebilirsin."
)

// Confirm is POST /v1/consents/confirm.
func (h *ConsentHandler) Confirm(c fiber.Ctx) error {
	outcome, err := h.svc.Confirm(c.Context(), tokenFrom(c))
	if err != nil {
		return linkErrorPage(c, err)
	}
	switch outcome {
	case consent.OutcomeEnded:
		return renderPage(c, fiber.StatusOK, page{Title: "Bu onay geri alınmış", Lines: []string{endedLine}})
	case consent.OutcomeSuperseded:
		return renderPage(c, fiber.StatusOK, page{Title: "Onayın zaten kayıtlı", Lines: []string{supersededLine}})
	case consent.OutcomeRenewed:
		return renderPage(c, fiber.StatusOK, page{
			Title: "Onayın sürüyor",
			Lines: []string{"Teşekkürler. Her e-postadaki bağlantıyla istediğin zaman geri alabilirsin."},
		})
	}
	return renderPage(c, fiber.StatusOK, page{
		Title: "Onayın kaydedildi",
		Lines: []string{"Teşekkürler. Her e-postadaki bağlantıyla istediğin zaman geri alabilirsin."},
	})
}

// textLinks are the Açık Rıza Metni and the aydınlatma metni.
func (h *ConsentHandler) textLinks() []pageLink {
	config := h.svc.Config()
	var links []pageLink
	if config.TextURL != "" {
		links = append(links, pageLink{Label: "Açık Rıza Metni", URL: config.TextURL})
	}
	if config.NoticeURL != "" {
		links = append(links, pageLink{Label: "KVKK Aydınlatma Metni", URL: config.NoticeURL})
	}
	return links
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
	Title string
	Lines []string
	// Quote is the consent text, shown set apart; After follows it.
	Quote string
	After []string
	Links []pageLink
	// Action, Token and Button make the one form of the page.
	Action string
	Token  string
	Button string
}

type pageLink struct {
	Label string
	URL   string
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
blockquote{margin:1rem 0;padding:.75rem 1rem;border-left:4px solid #1d4ed8;background:rgba(29,78,216,.06);line-height:1.5}
a{color:#1d4ed8}
button{font:inherit;padding:.7rem 1.4rem;border:0;border-radius:8px;background:#1d4ed8;color:#fff;cursor:pointer}
@media (prefers-color-scheme:dark){body{background:#11151c;color:#e6e9ef}main{background:#1b212b}a{color:#9db8ff}}
</style>
</head>
<body>
<main>
<h1>{{.Title}}</h1>
{{range .Lines}}<p>{{.}}</p>
{{end}}{{if .Quote}}<blockquote>{{.Quote}}</blockquote>
{{end}}{{range .After}}<p>{{.}}</p>
{{end}}{{if .Links}}<p>{{range $i, $l := .Links}}{{if $i}} · {{end}}<a href="{{$l.URL}}" rel="noopener noreferrer">{{$l.Label}}</a>{{end}}</p>
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

package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/commpref"
)

// CommunicationPreferenceHandler serves the Announcement preference
// (docs/communication-preferences.md): a person's own screen and changes, and the
// public unsubscribe page of every News mail.
type CommunicationPreferenceHandler struct {
	svc *commpref.Service
}

// NewCommunicationPreferenceHandler serves svc. A nil or disabled service answers
// every route 503.
func NewCommunicationPreferenceHandler(svc *commpref.Service) *CommunicationPreferenceHandler {
	return &CommunicationPreferenceHandler{svc: svc}
}

func communicationPreferenceError(c fiber.Ctx, err error) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	switch {
	case errors.Is(err, commpref.ErrDisabled):
		return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", "communication_preferences_unavailable")
	case errors.Is(err, commpref.ErrTeamsUnavailable):
		log.Printf("communication preferences: %v", err)
		c.Set(fiber.HeaderRetryAfter, "30")
		return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", "communication_preference_teams_unavailable")
	case errors.Is(err, commpref.ErrUnknownTeam):
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "communication_preference_team_unknown")
	case errors.Is(err, commpref.ErrInvalid):
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "communication_preference_invalid")
	case errors.Is(err, commpref.ErrFollowMembersOnly):
		return problemCode(c, fiber.StatusForbidden, "Forbidden", "communication_preference_follow_members_only")
	case errors.Is(err, commpref.ErrSubjectInactive):
		return problemCode(c, fiber.StatusForbidden, "Forbidden", "communication_preference_subject_inactive")
	}
	log.Printf("communication preferences: %v", err)
	return problem(c, fiber.StatusInternalServerError, "Internal Server Error")
}

// communicationPreferencePerson is the signed-in person (never a service account).
func communicationPreferencePerson(c fiber.Ctx) (authn.Identity, error) {
	ident, ok := c.Locals(authn.LocalsIdentity).(authn.Identity)
	if !ok {
		return authn.Identity{}, refuse(problem(c, fiber.StatusUnauthorized, "Unauthorized"))
	}
	if ident.ServiceAccount {
		return authn.Identity{}, refuse(problemCode(c, fiber.StatusForbidden, "Forbidden", "communication_preference_person_only"))
	}
	return ident, nil
}

// Mine is GET /v1/users/me/communication-preferences: the Account center's screen.
func (h *CommunicationPreferenceHandler) Mine(c fiber.Ctx) error {
	if !h.svc.Enabled() {
		return communicationPreferenceError(c, commpref.ErrDisabled)
	}
	ident, err := communicationPreferencePerson(c)
	if err != nil {
		return answered(err)
	}
	screen, err := h.svc.Mine(c.Context(), ident.ID)
	if err != nil {
		return communicationPreferenceError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(screen)
}

type communicationPreferenceItem struct {
	Category string          `json:"category"`
	Channel  string          `json:"channel"`
	Enabled  json.RawMessage `json:"enabled"`
}

type communicationPreferenceBody struct {
	Items []communicationPreferenceItem `json:"items"`
}

// Update is PUT /v1/users/me/communication-preferences: {"items":
// [{"category", "channel", "enabled": true|false|null}]}, null forgetting
// the choice. It answers the new screen.
func (h *CommunicationPreferenceHandler) Update(c fiber.Ctx) error {
	if !h.svc.Enabled() {
		return communicationPreferenceError(c, commpref.ErrDisabled)
	}
	ident, err := communicationPreferencePerson(c)
	if err != nil {
		return answered(err)
	}
	var body communicationPreferenceBody
	decoder := json.NewDecoder(bytes.NewReader(c.Body()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || len(body.Items) == 0 || len(body.Items) > commpref.MaxChanges {
		return communicationPreferenceError(c, commpref.ErrInvalid)
	}
	changes := make([]commpref.Change, 0, len(body.Items))
	for _, item := range body.Items {
		category, err := commpref.ParseCategory(item.Category)
		if err != nil {
			return communicationPreferenceError(c, err)
		}
		channel, err := commpref.ParseChannel(item.Channel)
		if err != nil {
			return communicationPreferenceError(c, err)
		}
		change := commpref.Change{Channel: channel, Category: category}
		switch strings.TrimSpace(string(item.Enabled)) {
		case "true":
			on := true
			change.Enabled = &on
		case "false":
			off := false
			change.Enabled = &off
		case "null":
		default:
			// Absent or anything else: the request must say what it wants.
			return communicationPreferenceError(c, commpref.ErrInvalid)
		}
		changes = append(changes, change)
	}
	screen, err := h.svc.Update(c.Context(), ident.ID, changes)
	if err != nil {
		return communicationPreferenceError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(screen)
}

// unsubscribeTokenFrom reads the link token: the query (an RFC 8058
// one-click POST goes to the List-Unsubscribe address as it is), else the
// page's form.
func unsubscribeTokenFrom(c fiber.Ctx) string {
	if token := strings.TrimSpace(c.Query("t")); token != "" {
		return token
	}
	return strings.TrimSpace(c.FormValue("t"))
}

// The reaches of the page's buttons.
const unsubscribeReachField = "kapsam"

// UnsubscribePage is GET /v1/communication-preferences/unsubscribe: it names the
// categories the link is for and asks before it acts, so a mail scanner
// that opens the link turns nothing off. No cookie, no redirect.
func (h *CommunicationPreferenceHandler) UnsubscribePage(c fiber.Ctx) error {
	if !h.svc.Enabled() {
		return unsubscribeErrorPage(c, commpref.ErrDisabled)
	}
	token := strings.TrimSpace(c.Query("t"))
	link, err := h.svc.ReadLink(c.Context(), token)
	if err != nil {
		return unsubscribeErrorPage(c, err)
	}
	labels := make([]string, 0, len(link.Categories))
	sources := make([]string, 0, len(link.Categories))
	for _, category := range link.Categories {
		labels = append(labels, category.Label)
		if !containsString(sources, category.Source) {
			sources = append(sources, category.Source)
		}
	}
	return renderPage(c, fiber.StatusOK, page{
		Title: "Bu postaları almak istemiyor musun?",
		Lines: []string{
			"Bu bağlantı şu postalar için: " + strings.Join(labels, ", ") + ".",
			"Yalnız bunları kapatabilir, bu kaynaktan gelen her şeyi kapatabilir ya da SKY LAB'ın bütün duyuru ve haber postalarından çıkabilirsin. Giriş kodu, bilet ve sertifika gibi hesap postaların bundan etkilenmez.",
		},
		Links:      []pageLink{{Label: "Tercihlerini Hesap Merkezi'nde yönet", URL: accountCenterLink(h.svc, link)}},
		Action:     commpref.UnsubscribePath,
		Token:      token,
		TokenField: "t",
		Choices: []pageChoice{
			{Name: unsubscribeReachField, Value: string(commpref.ReachCategories), Label: "Yalnız bunları kapat"},
			{Name: unsubscribeReachField, Value: string(commpref.ReachSources), Label: "Bu kaynaktan her şeyi kapat (" + strings.Join(sources, ", ") + ")"},
			{Name: unsubscribeReachField, Value: string(commpref.ReachAll), Label: "Tümünden çık"},
		},
	})
}

// accountCenterLink is the Account center page of the link's first
// category, which the screen shows first (bilet 09).
func accountCenterLink(svc *commpref.Service, link commpref.Link) string {
	if len(link.Categories) == 0 {
		return svc.Config().AccountCenterURL
	}
	return svc.Config().AccountCenterLink(link.Channel, link.Categories[0].Category)
}

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// Unsubscribe is POST /v1/communication-preferences/unsubscribe: a button of the
// page, or a mail client's one-click unsubscribe (RFC 8058: the body is
// List-Unsubscribe=One-Click, form-encoded or multipart, the token in the
// address), which turns off exactly the link's categories. It can be
// repeated: a second POST answers as the first.
func (h *CommunicationPreferenceHandler) Unsubscribe(c fiber.Ctx) error {
	oneClick := c.FormValue("List-Unsubscribe") == "One-Click"
	reach := commpref.ReachCategories
	if !oneClick {
		parsed, err := commpref.ParseReach(c.FormValue(unsubscribeReachField))
		if err != nil {
			return unsubscribeErrorPage(c, err)
		}
		reach = parsed
	}
	var done commpref.Link
	var err error
	if !h.svc.Enabled() {
		err = commpref.ErrDisabled
	} else {
		done, err = h.svc.Unsubscribe(c.Context(), unsubscribeTokenFrom(c), reach)
	}
	// A person being erased or erased gets no mail anyway: done.
	if errors.Is(err, commpref.ErrSubjectInactive) {
		err = nil
	}
	if oneClick {
		// RFC 8058 §3.2: no redirect, no page to act on.
		c.Set(fiber.HeaderCacheControl, "no-store")
		switch {
		case err == nil:
			return c.Status(fiber.StatusOK).SendString("ok")
		case errors.Is(err, commpref.ErrDisabled):
			return c.Status(fiber.StatusServiceUnavailable).SendString("unavailable")
		case errors.Is(err, commpref.ErrLink):
			return c.Status(fiber.StatusBadRequest).SendString("invalid link")
		}
		log.Printf("communication preferences: one-click unsubscribe: %v", err)
		return c.Status(fiber.StatusInternalServerError).SendString("error")
	}
	if err != nil {
		return unsubscribeErrorPage(c, err)
	}
	lines := []string{"Bu postalar artık sana gönderilmeyecek."}
	switch reach {
	case commpref.ReachSources:
		lines = []string{"Bu kaynaklardan gelen duyuru ve haber postaları artık sana gönderilmeyecek."}
	case commpref.ReachAll:
		lines = []string{"SKY LAB'ın duyuru ve haber postaları artık sana gönderilmeyecek. Hesap postaların (giriş kodu, bilet, sertifika) gelmeye devam eder."}
	}
	lines = append(lines, "Fikrini değiştirirsen tercihlerini Hesap Merkezi'nde yeniden açabilirsin.")
	return renderPage(c, fiber.StatusOK, page{
		Title: "Tercihin kaydedildi",
		Lines: lines,
		Links: []pageLink{{Label: "Hesap Merkezi", URL: accountCenterLink(h.svc, done)}},
	})
}

func unsubscribeErrorPage(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, commpref.ErrDisabled):
		return renderPage(c, fiber.StatusServiceUnavailable, page{
			Title: "Şu an yapılamıyor", Lines: []string{"Bu işlem şu an kullanılamıyor. Lütfen daha sonra yeniden dene."},
		})
	case errors.Is(err, commpref.ErrLink), errors.Is(err, commpref.ErrInvalid):
		return renderPage(c, fiber.StatusBadRequest, page{
			Title: "Bağlantı geçersiz", Lines: []string{"Bu bağlantı geçersiz. E-postadaki bağlantıyı eksiksiz açtığından emin ol."},
		})
	}
	log.Printf("communication preferences: unsubscribe: %v", err)
	return renderPage(c, fiber.StatusInternalServerError, page{
		Title: "Bir sorun oldu", Lines: []string{"İşlem tamamlanamadı. Lütfen biraz sonra yeniden dene."},
	})
}

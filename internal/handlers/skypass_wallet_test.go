package handlers

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- the RFC 6238 TOTP Google Wallet uses
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/googlewallet"
	"github.com/skylab-kulubu/core-backend/internal/skypass"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// walletGoogle is Google for the HTTP tests.
type walletGoogle struct {
	mu      sync.Mutex
	objects map[string]googlewallet.GenericObject
	down    bool
}

func (g *walletGoogle) SaveURL(_ []string, objects []googlewallet.ObjectRef) (string, error) {
	return googlewallet.SaveURLPrefix + "test." + objects[0].ID, nil
}

func (g *walletGoogle) EnsureGenericClass(context.Context, googlewallet.GenericClass) error {
	return nil
}

func (g *walletGoogle) InsertGenericObject(_ context.Context, object googlewallet.GenericObject) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.down {
		return &googlewallet.APIError{Op: "insert object", Status: http.StatusServiceUnavailable}
	}
	if _, ok := g.objects[object.ID]; ok {
		return &googlewallet.APIError{Op: "insert object", Status: http.StatusConflict}
	}
	g.objects[object.ID] = object
	return nil
}

func (g *walletGoogle) UpdateGenericObject(_ context.Context, object googlewallet.GenericObject) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.down {
		return &googlewallet.APIError{Op: "update object", Status: http.StatusServiceUnavailable}
	}
	g.objects[object.ID] = object
	return nil
}

func (g *walletGoogle) object(id string) googlewallet.GenericObject {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.objects[id]
}

var walletTestKey = bytes.Repeat([]byte{0x42}, 32)

// walletCode computes what a Wallet pass shows now, from the derivation
// docs/skypass-google-wallet.md describes, independently of the skypass
// package: HKDF-SHA256(key, info "skylab skypass google wallet totp
// v1\x00" + pass id) → 20 bytes, RFC 6238 SHA-1, 60 s, 8 digits.
func walletCode(t *testing.T, passID string, at time.Time) string {
	t.Helper()
	secret, err := hkdf.Key(sha256.New, walletTestKey, nil, "skylab skypass google wallet totp v1\x00"+passID, 20)
	if err != nil {
		t.Fatal(err)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(at.Unix()/60))
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	offset := sum[19] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("SPW1:%s:%08d", passID, value%100000000)
}

type walletHTTP struct {
	t       *testing.T
	google  *walletGoogle
	users   user.Store
	pass    skypass.Service
	tickets ticket.Service
	events  event.Store
	holder  authn.Identity
	// staff is door staff of one Event (roster B): settle only.
	staff authn.Identity
	// admin may also verify who a code belongs to (ticket:validate).
	admin authn.Identity
}

func newWalletHTTP(t *testing.T, enabled bool) *walletHTTP {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	w := &walletHTTP{
		t: t, google: &walletGoogle{objects: map[string]googlewallet.GenericObject{}},
		users: user.NewMemoryStore(), events: event.NewMemoryStore(),
		holder: authn.Identity{ID: uuid.MustParse("21212121-2121-2121-2121-212121212121"), Groups: []string{"/UYELER"}},
		staff:  authn.Identity{ID: uuid.MustParse("22222222-2121-2121-2121-212121212121"), Groups: []string{"/UYELER/ARGE/WEBLAB"}},
		admin:  authn.Identity{ID: uuid.MustParse("23232323-2121-2121-2121-212121212121"), Groups: []string{"/UYELER/YK"}},
	}
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	config := skypass.GoogleWalletConfig{}
	var api skypass.GoogleWalletAPI
	if enabled {
		config = skypass.GoogleWalletConfig{Enabled: true, IssuerID: "3388000000022222222", ClassSuffix: "skypass-test", TOTPKey: walletTestKey}
		api = w.google
	}
	wallet := skypass.NewWallet(config, api, skypass.NewMemoryWalletStore(), w.users, skypass.WalletOptions{})
	w.pass = skypass.NewServiceWithWallet(w.users, az, skypass.NewSigner(key, time.Minute), wallet)
	w.tickets = ticket.NewService(ticket.NewMemoryStore(), w.events, az, w.users)
	if _, _, err := user.NewService(w.users).Ensure(context.Background(), w.holder.ID, user.Profile{
		Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace",
	}); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *walletHTTP) app(ident authn.Identity) *fiber.App {
	h := NewSkyPassHandler(w.pass, w.tickets)
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		if ident.ID != uuid.Nil {
			c.Locals(authn.LocalsIdentity, ident)
		}
		return c.Next()
	})
	app.Get("/v1/skypass/wallet", h.WalletStatus)
	limit := SkyPassWalletLimit()
	app.Post("/v1/skypass/wallet/google", limit, h.GoogleWalletLink)
	app.Delete("/v1/skypass/wallet/google", limit, h.RevokeGoogleWallet)
	app.Post("/v1/skypass/verify", h.Verify)
	app.Post("/v1/sessions/:sessionId/check-in/skypass", h.CheckInSession)
	return app
}

func (w *walletHTTP) do(app *fiber.App, method, path, body string) (int, http.Header, map[string]any) {
	w.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(req)
	if err != nil {
		w.t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	return resp.StatusCode, resp.Header, parsed
}

func TestSkyPassWalletOffHTTP(t *testing.T) {
	t.Parallel()
	w := newWalletHTTP(t, false)
	app := w.app(w.holder)
	status, _, body := w.do(app, fiber.MethodGet, "/v1/skypass/wallet", "")
	google, _ := body["google"].(map[string]any)
	if status != fiber.StatusOK || google["available"] != false || google["issued"] != false {
		t.Fatalf("status %d %v", status, body)
	}
	for _, method := range []string{fiber.MethodPost, fiber.MethodDelete} {
		status, _, body := w.do(app, method, "/v1/skypass/wallet/google", "")
		if status != fiber.StatusServiceUnavailable || body["code"] != "skypass_google_wallet_off" {
			t.Fatalf("%s %d %v", method, status, body)
		}
	}
	// A Wallet-shaped code is not read: the same 400 as any other value.
	status, _, _ = w.do(w.app(w.admin), fiber.MethodPost, "/v1/skypass/verify",
		`{"token":"SPW1:ABCDEFGHIJKLMNOPQRSTUVWXYZ:12345678"}`)
	if status != fiber.StatusBadRequest {
		t.Fatalf("verify %d", status)
	}
	if status, _, _ := w.do(w.app(authn.Identity{}), fiber.MethodPost, "/v1/skypass/wallet/google", ""); status != fiber.StatusUnauthorized {
		t.Fatalf("anonymous %d", status)
	}
}

func TestSkyPassWalletLinkAndDoorHTTP(t *testing.T) {
	t.Parallel()
	w := newWalletHTTP(t, true)
	ctx := context.Background()
	member := w.app(w.holder)

	status, header, body := w.do(member, fiber.MethodPost, "/v1/skypass/wallet/google", "")
	saveURL, _ := body["saveUrl"].(string)
	if status != fiber.StatusOK || !strings.HasPrefix(saveURL, googlewallet.SaveURLPrefix) || header.Get("Cache-Control") != "no-store" {
		t.Fatalf("link %d %v", status, body)
	}
	objectID := strings.TrimPrefix(saveURL, googlewallet.SaveURLPrefix+"test.")
	passID := strings.TrimPrefix(objectID, "3388000000022222222.sp-")
	object := w.google.object(objectID)
	if object.RotatingBarcode == nil || object.RotatingBarcode.ValuePattern != "SPW1:"+passID+":{totp_value_0}" {
		t.Fatalf("object %+v", object)
	}

	// The door: an Event the staff member scans for, a Ticket for Ada.
	ev, err := w.events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", DoorStaffIDs: []uuid.UUID{w.staff.ID}})
	if err != nil {
		t.Fatal(err)
	}
	day, err := w.events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Day 1"})
	if err != nil {
		t.Fatal(err)
	}
	opening, err := w.events.CreateSession(ctx, event.Session{EventDayID: day.ID, Title: "Opening", SpeakerName: "Ada", SessionType: "PRESENTATION"})
	if err != nil {
		t.Fatal(err)
	}
	closing, err := w.events.CreateSession(ctx, event.Session{EventDayID: day.ID, Title: "Closing", SpeakerName: "Ada", SessionType: "PRESENTATION"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.tickets.Apply(ctx, authz.Principal{ID: w.holder.ID.String()}, ev.ID); err != nil {
		t.Fatal(err)
	}
	door := w.app(w.staff)
	code := walletCode(t, passID, time.Now())

	status, _, body = w.do(w.app(w.admin), fiber.MethodPost, "/v1/skypass/verify", `{"token":`+jsonString(code)+`}`)
	if status != fiber.StatusOK || body["firstName"] != "Ada" || body["id"] != w.holder.ID.String() {
		t.Fatalf("verify %d %v", status, body)
	}
	status, _, body = w.do(door, fiber.MethodPost, "/v1/sessions/"+opening.ID.String()+"/check-in/skypass", `{"token":`+jsonString(code)+`}`)
	if status != fiber.StatusCreated || body["sessionId"] != opening.ID.String() {
		t.Fatalf("settle %d %v", status, body)
	}
	// The same code at another Oturum: spent.
	status, _, body = w.do(door, fiber.MethodPost, "/v1/sessions/"+closing.ID.String()+"/check-in/skypass", `{"token":`+jsonString(code)+`}`)
	if status != fiber.StatusConflict || body["code"] != "skypass_wallet_code_used" {
		t.Fatalf("replay %d %v", status, body)
	}

	// Lost phone: the member revokes; the pass's codes stop.
	if status, _, _ := w.do(member, fiber.MethodDelete, "/v1/skypass/wallet/google", ""); status != fiber.StatusNoContent {
		t.Fatalf("revoke %d", status)
	}
	if got := w.google.object(objectID); got.State != "INACTIVE" || got.RotatingBarcode != nil {
		t.Fatalf("google's copy %+v", got)
	}
	status, _, body = w.do(member, fiber.MethodGet, "/v1/skypass/wallet", "")
	if google, _ := body["google"].(map[string]any); status != fiber.StatusOK || google["available"] != true || google["issued"] != false {
		t.Fatalf("status after revoke %d %v", status, body)
	}
}

func TestSkyPassWalletErrorsHTTP(t *testing.T) {
	t.Parallel()
	w := newWalletHTTP(t, true)
	member := w.app(w.holder)
	w.google.mu.Lock()
	w.google.down = true
	w.google.mu.Unlock()
	status, header, body := w.do(member, fiber.MethodPost, "/v1/skypass/wallet/google", "")
	if status != fiber.StatusBadGateway || body["code"] != "skypass_google_wallet_unavailable" || header.Get("Retry-After") == "" {
		t.Fatalf("google down %d %v", status, body)
	}
	w.google.mu.Lock()
	w.google.down = false
	w.google.mu.Unlock()
	_, _, body = w.do(member, fiber.MethodPost, "/v1/skypass/wallet/google", "")
	saveURL, _ := body["saveUrl"].(string)
	passID := strings.TrimPrefix(saveURL, googlewallet.SaveURLPrefix+"test.3388000000022222222.sp-")

	door := w.app(w.admin)
	right := walletCode(t, passID, time.Now())
	wrong := "SPW1:" + passID + ":00000000"
	if wrong == right {
		wrong = "SPW1:" + passID + ":00000001"
	}
	for range skypass.DefaultWalletFailureLimit {
		if status, _, _ := w.do(door, fiber.MethodPost, "/v1/skypass/verify", `{"token":`+jsonString(wrong)+`}`); status != fiber.StatusBadRequest {
			t.Fatalf("wrong code %d", status)
		}
	}
	status, header, body = w.do(door, fiber.MethodPost, "/v1/skypass/verify", `{"token":`+jsonString(right)+`}`)
	if status != fiber.StatusTooManyRequests || body["code"] != "skypass_wallet_rate_limited" || header.Get("Retry-After") == "" {
		t.Fatalf("limited %d %v %v", status, header, body)
	}

	// Ten writes a minute per person.
	for i := 0; ; i++ {
		status, _, body := w.do(member, fiber.MethodPost, "/v1/skypass/wallet/google", "")
		if status == fiber.StatusTooManyRequests {
			if body["code"] != "skypass_wallet_link_rate_limited" || i > SkyPassWalletLinksPerMinute {
				t.Fatalf("link limit after %d: %v", i, body)
			}
			break
		}
		if i > SkyPassWalletLinksPerMinute+1 {
			t.Fatal("no link limit")
		}
	}
}

// Only who may take check-ins at a Session's door may spend a code there:
// a member who photographs the pass in the queue, or another Event's door
// staff, gets 403 before any code is read, and the holder's code still works
// at the real door. The answer does not depend on the code either, so it
// tells nothing about the holder.
func TestSkyPassCheckInRefusesWhoCannotUseTheDoorBeforeSpendingTheCode(t *testing.T) {
	t.Parallel()
	w := newWalletHTTP(t, true)
	ctx := context.Background()
	_, _, body := w.do(w.app(w.holder), fiber.MethodPost, "/v1/skypass/wallet/google", "")
	saveURL, _ := body["saveUrl"].(string)
	passID := strings.TrimPrefix(saveURL, googlewallet.SaveURLPrefix+"test.3388000000022222222.sp-")

	ev, err := w.events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", DoorStaffIDs: []uuid.UUID{w.staff.ID}})
	if err != nil {
		t.Fatal(err)
	}
	day, err := w.events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Day 1"})
	if err != nil {
		t.Fatal(err)
	}
	opening, err := w.events.CreateSession(ctx, event.Session{EventDayID: day.ID, Title: "Opening", SpeakerName: "Ada", SessionType: "PRESENTATION"})
	if err != nil {
		t.Fatal(err)
	}
	closing, err := w.events.CreateSession(ctx, event.Session{EventDayID: day.ID, Title: "Closing", SpeakerName: "Ada", SessionType: "PRESENTATION"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.tickets.Apply(ctx, authz.Principal{ID: w.holder.ID.String()}, ev.ID); err != nil {
		t.Fatal(err)
	}
	// Another Event, with its own door staff and no Ticket of Ada's.
	otherStaff := authn.Identity{ID: uuid.MustParse("24242424-2121-2121-2121-212121212121"), Groups: []string{"/UYELER/ARGE/GAMELAB"}}
	other, err := w.events.Create(ctx, event.Event{Name: "Jam", Location: "YTÜ", OwnerTeam: "GAMELAB", DoorStaffIDs: []uuid.UUID{otherStaff.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.events.CreateDay(ctx, event.Day{EventID: other.ID, Name: "Day 1"}); err != nil {
		t.Fatal(err)
	}

	outsider := authn.Identity{ID: uuid.MustParse("25252525-2121-2121-2121-212121212121"), Groups: []string{"/UYELER"}}
	if _, _, err := user.NewService(w.users).Ensure(ctx, outsider.ID, user.Profile{Email: "mal@example.com", FirstName: "Mal", LastName: "Lory"}); err != nil {
		t.Fatal(err)
	}
	code := walletCode(t, passID, time.Now())
	wrong := "SPW1:" + passID + ":00000000"
	if wrong == code {
		wrong = "SPW1:" + passID + ":00000001"
	}
	openingPath := "/v1/sessions/" + opening.ID.String() + "/check-in/skypass"
	for name, ident := range map[string]authn.Identity{"member": outsider, "other event's door staff": otherStaff} {
		for _, token := range []string{code, wrong} {
			status, _, body := w.do(w.app(ident), fiber.MethodPost, openingPath, `{"token":`+jsonString(token)+`}`)
			if status != fiber.StatusForbidden {
				t.Fatalf("%s: %d %v", name, status, body)
			}
		}
	}
	// No such Session: 404, before any code is read.
	if status, _, body := w.do(w.app(outsider), fiber.MethodPost, "/v1/sessions/"+uuid.NewString()+"/check-in/skypass", `{"token":`+jsonString(code)+`}`); status != fiber.StatusNotFound {
		t.Fatalf("unknown session: %d %v", status, body)
	}
	// An in-app token of someone with no Ticket here: 403 as well, not 404.
	stranger, err := w.pass.Mint(ctx, authz.Principal{ID: outsider.ID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if status, _, body := w.do(w.app(outsider), fiber.MethodPost, openingPath, `{"token":`+jsonString(stranger.Value)+`}`); status != fiber.StatusForbidden {
		t.Fatalf("in-app token: %d %v", status, body)
	}

	// The real door: the code was not spent, and it checks in once.
	door := w.app(w.staff)
	status, _, body := w.do(door, fiber.MethodPost, openingPath, `{"token":`+jsonString(code)+`}`)
	if status != fiber.StatusCreated || body["sessionId"] != opening.ID.String() {
		t.Fatalf("door staff %d %v", status, body)
	}
	status, _, body = w.do(door, fiber.MethodPost, "/v1/sessions/"+closing.ID.String()+"/check-in/skypass", `{"token":`+jsonString(code)+`}`)
	if status != fiber.StatusConflict || body["code"] != "skypass_wallet_code_used" {
		t.Fatalf("second use %d %v", status, body)
	}
}

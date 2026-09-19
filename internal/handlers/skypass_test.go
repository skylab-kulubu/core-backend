package handlers

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/middlewares"
	"github.com/skylab-kulubu/core-backend/internal/skypass"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func skypassApp(t *testing.T, ident authn.Identity, store user.Store, signer *skypass.Signer) *fiber.App {
	t.Helper()
	if store == nil {
		store = user.NewMemoryStore()
	}
	if signer == nil {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		signer = skypass.NewSigner(key, time.Minute)
	}
	svc := user.NewService(store)
	jit := middlewares.NewJIT(svc)
	h := NewSkyPassHandler(skypass.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()), signer), nil)
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Get("/v1/skypass/jwks", h.JWKS)
	app.Use(func(c fiber.Ctx) error {
		if ident.ID != uuid.Nil || len(ident.Groups) > 0 {
			c.Locals(authn.LocalsIdentity, ident)
		}
		return c.Next()
	})
	app.Use(jit.Handle)
	app.Post("/v1/skypass/card-bind", h.BindCard)
	app.Get("/v1/skypass/card", h.LookupCard)
	app.Post("/v1/skypass/qr", h.Mint)
	app.Post("/v1/skypass/verify", h.Verify)
	return app
}

func TestSkyPassBindUniqueConflictHTTP(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	aliceID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	bobID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	alice := authn.Identity{ID: aliceID, Profile: user.Profile{Email: "alice@example.com", FirstName: "Alice", LastName: "A"}}
	app := skypassApp(t, alice, store, nil)

	req := httptest.NewRequest(fiber.MethodPost, "/v1/skypass/card-bind", strings.NewReader(`{"uid":"04AABBCCDD"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("bind status %d body %s", resp.StatusCode, body)
	}

	bob := authn.Identity{ID: bobID, Profile: user.Profile{Email: "bob@example.com", FirstName: "Bob", LastName: "B"}}
	app = skypassApp(t, bob, store, nil)
	req = httptest.NewRequest(fiber.MethodPost, "/v1/skypass/card-bind", strings.NewReader(`{"uid":"04AABBCCDD"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("conflict status %d body %s", resp.StatusCode, body)
	}
}

func TestSkyPassEmptyUIDWipeDoesNotStealHTTP(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	aliceID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	bobID := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	alice := authn.Identity{ID: aliceID, Profile: user.Profile{Email: "alice@example.com", FirstName: "Alice", LastName: "A"}}
	app := skypassApp(t, alice, store, nil)
	req := httptest.NewRequest(fiber.MethodPost, "/v1/skypass/card-bind", strings.NewReader(`{"uid":"04CAFEBABE"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("alice bind %d", resp.StatusCode)
	}

	bob := authn.Identity{ID: bobID, Profile: user.Profile{Email: "bob@example.com", FirstName: "Bob", LastName: "B"}}
	app = skypassApp(t, bob, store, nil)
	req = httptest.NewRequest(fiber.MethodPost, "/v1/skypass/card-bind", strings.NewReader(`{"uid":""}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("bob wipe %d %s", resp.StatusCode, body)
	}
	var wiped user.User
	if err := json.NewDecoder(resp.Body).Decode(&wiped); err != nil {
		t.Fatal(err)
	}
	if wiped.ID != bobID || wiped.StudentCardUID != "" {
		t.Fatalf("wipe %+v", wiped)
	}

	yk := authn.Identity{ID: aliceID, Profile: user.Profile{Email: "alice@example.com"}, Groups: []string{"/UYELER/YK"}}
	app = skypassApp(t, yk, store, nil)
	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/skypass/card?uid=04CAFEBABE", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("lookup %d %s", resp.StatusCode, body)
	}
	var hit skypass.Holder
	if err := json.NewDecoder(resp.Body).Decode(&hit); err != nil {
		t.Fatal(err)
	}
	if hit.ID != aliceID {
		t.Fatalf("stole %+v", hit)
	}
}

func TestSkyPassVerifyRejectsExpiredAndUnsignedHTTP(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer := skypass.NewSigner(key, time.Minute)
	frozen := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	signer.Now = func() time.Time { return frozen }
	id := uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	yk := authn.Identity{ID: id, Profile: user.Profile{Email: "yk@example.com", FirstName: "Ada", LastName: "Lovelace"}, Groups: []string{"/UYELER/YK"}}
	app := skypassApp(t, yk, store, signer)

	req := httptest.NewRequest(fiber.MethodPost, "/v1/skypass/qr", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("mint %d %s", resp.StatusCode, body)
	}
	var minted skypass.Token
	if err := json.NewDecoder(resp.Body).Decode(&minted); err != nil {
		t.Fatal(err)
	}
	if minted.Value == "" || strings.HasPrefix(minted.Value, "SKYPASS:") {
		t.Fatalf("token %q", minted.Value)
	}

	verify := func(raw string) *httptest.ResponseRecorder {
		t.Helper()
		body := `{"token":` + jsonString(raw) + `}`
		r := httptest.NewRequest(fiber.MethodPost, "/v1/skypass/verify", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(r)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		rec.Code = resp.StatusCode
		b, _ := io.ReadAll(resp.Body)
		_, _ = rec.Body.Write(b)
		return rec
	}

	ok := verify(minted.Value)
	if ok.Code != fiber.StatusOK {
		t.Fatalf("valid verify %d %s", ok.Code, ok.Body.String())
	}
	var holder skypass.Holder
	if err := json.Unmarshal(ok.Body.Bytes(), &holder); err != nil {
		t.Fatal(err)
	}
	if holder.ID != id || holder.SkyNumber == "" {
		t.Fatalf("holder %+v", holder)
	}

	signer.Now = func() time.Time { return frozen.Add(2 * time.Minute) }
	expired := verify(minted.Value)
	if expired.Code != fiber.StatusUnauthorized {
		t.Fatalf("expired %d %s", expired.Code, expired.Body.String())
	}

	unsigned := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"sub": id.String(), "exp": frozen.Add(time.Hour).Unix(),
	})
	raw, err := unsigned.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	bad := verify(raw)
	if bad.Code == fiber.StatusOK {
		t.Fatalf("unsigned accepted %s", bad.Body.String())
	}
	plain := verify("SKYPASS:SKY-0000001:Ada Lovelace")
	if plain.Code == fiber.StatusOK {
		t.Fatalf("plaintext accepted")
	}
}

func TestSkyPassJWKSAnonymousHTTP(t *testing.T) {
	t.Parallel()
	app := skypassApp(t, authn.Identity{}, user.NewMemoryStore(), nil)
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/skypass/jwks", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("jwks %d", resp.StatusCode)
	}
	var doc skypass.JWKS
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Keys) != 1 || doc.Keys[0].Kty != "EC" || doc.Keys[0].Alg != "ES256" || doc.Keys[0].Crv != "P-256" {
		t.Fatalf("jwks %+v", doc)
	}
}

func TestSkyPassLookupForbiddenForMemberHTTP(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	id := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	member := authn.Identity{ID: id, Profile: user.Profile{Email: "m@example.com", FirstName: "M", LastName: "M"}, Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	app := skypassApp(t, member, store, nil)
	req := httptest.NewRequest(fiber.MethodPost, "/v1/skypass/card-bind", strings.NewReader(`{"uid":"04ABCDABCD"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("bind %d", resp.StatusCode)
	}
	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/skypass/card?uid=04ABCDABCD", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("lookup %d %s", resp.StatusCode, body)
	}
}

func TestSkyPassBindUnauthorizedHTTP(t *testing.T) {
	t.Parallel()
	app := skypassApp(t, authn.Identity{}, user.NewMemoryStore(), nil)
	req := httptest.NewRequest(fiber.MethodPost, "/v1/skypass/card-bind", strings.NewReader(`{"uid":"04AABBCCDD"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestSkyPassPrivilegedUnbindHTTP(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	memberID := uuid.MustParse("12121212-1212-1212-1212-121212121212")
	ykID := uuid.MustParse("13131313-1313-1313-1313-131313131313")
	member := authn.Identity{ID: memberID, Profile: user.Profile{Email: "m@example.com", FirstName: "M", LastName: "M"}, Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	app := skypassApp(t, member, store, nil)
	req := httptest.NewRequest(fiber.MethodPost, "/v1/skypass/card-bind", strings.NewReader(`{"uid":"04DEADBEEF"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("bind %d", resp.StatusCode)
	}

	stranger := authn.Identity{ID: ykID, Profile: user.Profile{Email: "s@example.com", FirstName: "S", LastName: "S"}, Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	app = skypassApp(t, stranger, store, nil)
	req = httptest.NewRequest(fiber.MethodPost, "/v1/skypass/card-bind", strings.NewReader(`{"uid":"","userId":"`+memberID.String()+`"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("member unbind other %d %s", resp.StatusCode, body)
	}

	yk := authn.Identity{ID: ykID, Profile: user.Profile{Email: "yk@example.com", FirstName: "Y", LastName: "K"}, Groups: []string{"/UYELER/YK"}}
	app = skypassApp(t, yk, store, nil)
	req = httptest.NewRequest(fiber.MethodPost, "/v1/skypass/card-bind", strings.NewReader(`{"uid":"","userId":"`+memberID.String()+`"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("yk unbind %d %s", resp.StatusCode, body)
	}
	var got user.User
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.ID != memberID || got.StudentCardUID != "" {
		t.Fatalf("unbind %+v", got)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestSkyPassCheckInSessionHTTP(t *testing.T) {
	t.Parallel()
	users := user.NewMemoryStore()
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer := skypass.NewSigner(key, time.Minute)
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	ticketSvc := ticket.NewService(tickets, events, az, users)
	holderID := uuid.MustParse("15151515-1515-1515-1515-151515151515")
	staffID := uuid.MustParse("16161616-1616-1616-1616-161616161616")
	if _, _, err := user.NewService(users).Ensure(t.Context(), holderID, user.Profile{Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"}); err != nil {
		t.Fatal(err)
	}
	pass := skypass.NewService(users, az, signer)
	if _, err := pass.BindCard(t.Context(), authz.Principal{ID: holderID.String()}, "04FEEDFACE", nil); err != nil {
		t.Fatal(err)
	}
	ev, err := events.Create(t.Context(), event.Event{
		Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", DoorStaffIDs: []uuid.UUID{staffID},
	})
	if err != nil {
		t.Fatal(err)
	}
	day, err := events.CreateDay(t.Context(), event.Day{EventID: ev.ID, Name: "Day 1"})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := events.CreateSession(t.Context(), event.Session{
		EventDayID: day.ID, Title: "Opening", SpeakerName: "Ada", SessionType: "PRESENTATION",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ticketSvc.Apply(t.Context(), authz.Principal{ID: holderID.String()}, ev.ID); err != nil {
		t.Fatal(err)
	}
	second, err := events.CreateSession(t.Context(), event.Session{
		EventDayID: day.ID, Title: "Closing", SpeakerName: "Ada", SessionType: "PRESENTATION",
	})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := pass.Mint(t.Context(), authz.Principal{ID: holderID.String()})
	if err != nil {
		t.Fatal(err)
	}

	staff := authn.Identity{ID: staffID, Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	h := NewSkyPassHandler(pass, ticketSvc)
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, staff)
		return c.Next()
	})
	app.Post("/v1/sessions/:sessionId/check-in/skypass", h.CheckInSession)
	path := "/v1/sessions/" + sess.ID.String() + "/check-in/skypass"

	req := httptest.NewRequest(fiber.MethodPost, path, strings.NewReader(`{"token":`+jsonString(tok.Value)+`}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("token settle %d %s", resp.StatusCode, body)
	}
	var created ticket.CheckIn
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.SessionID != sess.ID {
		t.Fatalf("created %+v", created)
	}

	uidPath := "/v1/sessions/" + second.ID.String() + "/check-in/skypass"
	uidReq := httptest.NewRequest(fiber.MethodPost, uidPath, strings.NewReader(`{"uid":"04FEEDFACE"}`))
	uidReq.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(uidReq)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("uid settle %d %s", resp.StatusCode, body)
	}

	dup := httptest.NewRequest(fiber.MethodPost, path, strings.NewReader(`{"uid":"04FEEDFACE"}`))
	dup.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(dup)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("dup %d %s", resp.StatusCode, body)
	}

	memberApp := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	memberApp.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{
			ID: uuid.MustParse("17171717-1717-1717-1717-171717171717"), Groups: []string{"/UYELER/ARGE/WEBLAB"},
		})
		return c.Next()
	})
	memberApp.Post("/v1/sessions/:sessionId/check-in/skypass", h.CheckInSession)
	memberReq := httptest.NewRequest(fiber.MethodPost, path, strings.NewReader(`{"uid":"04FEEDFACE"}`))
	memberReq.Header.Set("Content-Type", "application/json")
	resp, err = memberApp.Test(memberReq)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("member %d %s", resp.StatusCode, body)
	}
}

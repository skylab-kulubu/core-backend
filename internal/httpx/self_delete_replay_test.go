package httpx_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/accessgate"
	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// sessionRealm answers RFC 7662 introspection the way Keycloak does for the
// sky-account sudo tokens it minted: active while the user session the token
// is bound to exists, `{"active":false}` once that session is gone.
type sessionRealm struct {
	issuer string
	mu     sync.Mutex
	tokens map[string]jwt.MapClaims
	closed map[string]bool
	calls  atomic.Int32
}

func newSessionRealm(issuer string) *sessionRealm {
	return &sessionRealm{issuer: issuer, tokens: map[string]jwt.MapClaims{}, closed: map[string]bool{}}
}

func (r *sessionRealm) mintSudo(token string, subject uuid.UUID, sessionID string, now time.Time) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens[token] = jwt.MapClaims{
		"active": true, "typ": "sky-sudo", "iss": r.issuer, "sub": subject.String(),
		"azp": "account-center", "aud": []any{"sky-account", "core"}, "sid": sessionID,
		"exp": float64(now.Add(5 * time.Minute).Unix()),
	}
	return token
}

func (r *sessionRealm) closeSession(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed[sessionID] = true
}

func (r *sessionRealm) Introspect(_ context.Context, token string) (map[string]any, error) {
	r.calls.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	claims, ok := r.tokens[token]
	if !ok || r.closed[claims["sid"].(string)] {
		return map[string]any{"active": false}, nil
	}
	return claims, nil
}

// markerGate is the shared access gate: a subject is blocked once Core has
// written its permanent deletion marker.
type markerGate struct {
	mu      sync.Mutex
	blocked map[string]bool
}

func (g *markerGate) Check(_ context.Context, subject string) accessgate.Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.blocked[subject] {
		return accessgate.Blocked
	}
	return accessgate.Allowed
}

func (g *markerGate) Ready(context.Context) error { return nil }

type markingProjector struct {
	store *user.MemoryStore
	gate  *markerGate
}

func (p markingProjector) Project(ctx context.Context, request user.DeletionRequest) error {
	p.gate.mu.Lock()
	p.gate.blocked[request.SubjectID.String()] = true
	p.gate.mu.Unlock()
	return p.store.MarkDeletionPlatformBlocked(ctx, request.ID, time.Now().UTC())
}

// A7c through the assembled app: Core accepts the deletion, the answer is
// lost, the saga closes the person's Keycloak session and the access gate
// blocks the subject everywhere else. Account Center's retry with the same
// idempotency key, bearer and (now inactive) sudo proof still reads the
// accepted outcome: the self-delete route sits before the gate, and the
// replay is answered from the stored request before the realm is asked.
func TestSelfDeleteReplayOutlivesTheClosedSessionAndTheAccessGate(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	now := time.Now().UTC().Truncate(time.Second)
	realm := newSessionRealm(keys.Issuer)
	gate := &markerGate{blocked: map[string]bool{}}
	store := user.NewMemoryStore()
	selfDeletion, err := account.NewSelfDeletion(store, markingProjector{store: store, gate: gate}, account.SelfDeletionConfig{
		Enabled: true, ReceiptKey: []byte("0123456789abcdef0123456789abcdef"), ReceiptTTL: 90 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	app := httpx.New(httpx.Deps{
		ParseToken:        keys.Parse(),
		AccountAccessGate: gate,
		SelfDeletion:      selfDeletion,
		ParseSelfDeleteContext: func(accessToken, idToken string) (authn.Identity, error) {
			return authn.ParseSelfDeleteContext(accessToken, idToken, keys.Verify, keys.Issuer, "account-center", time.Now().UTC(), 5*time.Minute)
		},
		ParseSelfDeleteSudo: func(ctx context.Context, accessToken, sudoToken string) (authn.Identity, error) {
			return authn.ParseSelfDeleteSudoContext(ctx, accessToken, sudoToken, keys.Verify, realm, keys.Issuer, "account-center", time.Now().UTC())
		},
		ParseSelfDeleteBearer: func(accessToken string) (authn.Identity, error) {
			return authn.ParseSelfDeleteBearer(accessToken, keys.Verify, keys.Issuer, "account-center", time.Now().UTC())
		},
	})

	person := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	stranger := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	bearer := func(subject uuid.UUID, sessionID string, expiresAt time.Time) string {
		return keys.Sign(t, jwt.MapClaims{
			"sub": subject.String(), "iss": keys.Issuer, "aud": []string{"account", "core"},
			"azp": "account-center", "scope": "openid", "sid": sessionID, "exp": expiresAt.Unix(),
		})
	}
	idToken := func(authenticatedAt time.Time) string {
		return keys.Sign(t, jwt.MapClaims{
			"sub": person.String(), "iss": keys.Issuer, "aud": "account-center", "sid": "session-a",
			"auth_time": authenticatedAt.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		})
	}
	personBearer := bearer(person, "session-a", now.Add(5*time.Minute))
	sudo := realm.mintSudo("sudo-a", person, "session-a", now)
	key := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	newKey := base64.RawURLEncoding.EncodeToString([]byte("abcdef0123456789abcdef0123456789"))

	post := func(headers map[string]string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(fiber.MethodPost, "/v1/account-deletion-requests/self", nil)
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, body
	}

	status, accepted := post(map[string]string{
		"Authorization": "Bearer " + personBearer, "X-Sky-Sudo": sudo, "Idempotency-Key": key,
	})
	if status != fiber.StatusAccepted || accepted["platformBlocked"] != true || accepted["receipt"] == nil {
		t.Fatalf("accept status=%d body=%#v", status, accepted)
	}
	if realm.calls.Load() != 1 {
		t.Fatalf("introspections after accept = %d", realm.calls.Load())
	}

	// The marker is in place: the same person is refused by every gated route.
	me := httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil)
	me.Header.Set(fiber.HeaderAuthorization, "Bearer "+keys.Token(t, jwt.MapClaims{"sub": person.String()}))
	if resp, err := app.Test(me); err != nil || resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("gated route for a blocked subject: resp=%v err=%v", resp, err)
	}

	// The saga closes the session; the realm now calls the proof inactive.
	realm.closeSession("session-a")

	status, replayed := post(map[string]string{
		"Authorization": "Bearer " + personBearer, "X-Sky-Sudo": sudo, "Idempotency-Key": key,
	})
	if status != fiber.StatusAccepted || !reflect.DeepEqual(replayed, accepted) {
		t.Fatalf("replay status=%d body=%#v, want %#v", status, replayed, accepted)
	}
	if realm.calls.Load() != 1 {
		t.Fatalf("the replay asked the realm: %d introspections", realm.calls.Load())
	}

	for name, headers := range map[string]map[string]string{
		"same key, another subject": {
			"Authorization": "Bearer " + bearer(stranger, "session-b", now.Add(5*time.Minute)), "X-Sky-Sudo": sudo, "Idempotency-Key": key,
		},
		"new key, inactive proof": {
			"Authorization": "Bearer " + personBearer, "X-Sky-Sudo": sudo, "Idempotency-Key": newKey,
		},
		"same key, expired bearer": {
			"Authorization": "Bearer " + bearer(person, "session-a", now.Add(-time.Second)), "X-Sky-Sudo": sudo, "Idempotency-Key": key,
		},
		"same key, bearer of another client": {
			"Authorization": "Bearer " + keys.Token(t, jwt.MapClaims{"sub": person.String(), "azp": "skyforms", "sid": "session-a", "scope": "openid"}),
			"X-Sky-Sudo":    sudo, "Idempotency-Key": key,
		},
		"same key, stale ID token": {
			"Authorization": "Bearer " + personBearer, "X-Account-Reauth-Token": idToken(now.Add(-10 * time.Minute)), "Idempotency-Key": key,
		},
	} {
		status, problem := post(headers)
		if status != fiber.StatusUnauthorized || problem["code"] != "invalid_end_user_token" {
			t.Fatalf("%s: status=%d body=%#v", name, status, problem)
		}
	}

	// The old ID-token proof is verified locally and does not depend on the
	// session, so its retry goes through Begin exactly as before.
	status, viaIDToken := post(map[string]string{
		"Authorization": "Bearer " + personBearer, "X-Account-Reauth-Token": idToken(now), "Idempotency-Key": key,
	})
	if status != fiber.StatusAccepted || !reflect.DeepEqual(viaIDToken, accepted) {
		t.Fatalf("id-token retry status=%d body=%#v", status, viaIDToken)
	}
	if _, err := store.DeletionRequest(context.Background(), stranger); err == nil {
		t.Fatal("a refused call created a request for another subject")
	}
}

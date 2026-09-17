package skypass

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func seedUser(t *testing.T, store user.Store, id uuid.UUID, email, first, last string) user.User {
	t.Helper()
	got, _, err := user.NewService(store).Ensure(context.Background(), id, user.Profile{
		Email: email, FirstName: first, LastName: last,
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func newSvc(t *testing.T, store user.Store, ttl time.Duration) Service {
	t.Helper()
	return NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()), NewSigner(testKey(t), ttl))
}

func TestService_BindUniqueConflict(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	svc := newSvc(t, store, time.Minute)
	ctx := context.Background()
	aliceID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	bobID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	seedUser(t, store, aliceID, "alice@example.com", "Alice", "A")
	seedUser(t, store, bobID, "bob@example.com", "Bob", "B")

	alice := authz.Principal{ID: aliceID.String()}
	bob := authz.Principal{ID: bobID.String()}
	got, err := svc.BindCard(ctx, alice, "04aa:bb:cc:dd", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.StudentCardUID != "04AABBCCDD" {
		t.Fatalf("bound %+v", got)
	}

	_, err = svc.BindCard(ctx, bob, "04AABBCCDD", nil)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("got %v", err)
	}
	still, err := svc.Lookup(ctx, authz.Principal{ID: aliceID.String(), Groups: []string{"/UYELER/YK"}}, "04AABBCCDD")
	if err != nil {
		t.Fatal(err)
	}
	if still.ID != aliceID {
		t.Fatalf("stole card %+v", still)
	}
}

func TestService_RebindReplacesOldUID(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	svc := newSvc(t, store, time.Minute)
	ctx := context.Background()
	id := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	seedUser(t, store, id, "ada@example.com", "Ada", "Lovelace")
	p := authz.Principal{ID: id.String()}
	staff := authz.Principal{ID: id.String(), Groups: []string{"/UYELER/YK"}}

	if _, err := svc.BindCard(ctx, p, "04AAAAAAAA", nil); err != nil {
		t.Fatal(err)
	}
	got, err := svc.BindCard(ctx, p, "04BBBBBBBB", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.StudentCardUID != "04BBBBBBBB" {
		t.Fatalf("rebind %+v", got)
	}
	if _, err := svc.Lookup(ctx, staff, "04AAAAAAAA"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old uid %v", err)
	}
	hit, err := svc.Lookup(ctx, staff, "04BBBBBBBB")
	if err != nil || hit.ID != id {
		t.Fatalf("new uid %v %+v", err, hit)
	}
}

func TestService_EmptyUIDWipeDoesNotStealCard(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	svc := newSvc(t, store, time.Minute)
	ctx := context.Background()
	aliceID := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	bobID := uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	seedUser(t, store, aliceID, "alice@example.com", "Alice", "A")
	seedUser(t, store, bobID, "bob@example.com", "Bob", "B")
	alice := authz.Principal{ID: aliceID.String()}
	bob := authz.Principal{ID: bobID.String()}
	staff := authz.Principal{ID: aliceID.String(), Groups: []string{"/UYELER/YK"}}

	if _, err := svc.BindCard(ctx, alice, "04CAFEBABE", nil); err != nil {
		t.Fatal(err)
	}
	wiped, err := svc.BindCard(ctx, bob, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if wiped.StudentCardUID != "" || wiped.ID != bobID {
		t.Fatalf("wipe %+v", wiped)
	}
	hit, err := svc.Lookup(ctx, staff, "04CAFEBABE")
	if err != nil || hit.ID != aliceID {
		t.Fatalf("alice lost card %v %+v", err, hit)
	}
	again, err := svc.BindCard(ctx, alice, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.StudentCardUID != "" {
		t.Fatalf("alice unbind %+v", again)
	}
	if _, err := svc.Lookup(ctx, staff, "04CAFEBABE"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unbound lookup %v", err)
	}
}

func TestService_PrivilegedUnbindOther(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	svc := newSvc(t, store, time.Minute)
	ctx := context.Background()
	memberID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	otherID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	ykID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	seedUser(t, store, memberID, "m@example.com", "Mem", "Ber")
	seedUser(t, store, otherID, "o@example.com", "Oth", "Er")
	seedUser(t, store, ykID, "yk@example.com", "Y", "K")
	member := authz.Principal{ID: memberID.String(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	yk := authz.Principal{ID: ykID.String(), Groups: []string{"/UYELER/YK"}}

	if _, err := svc.BindCard(ctx, authz.Principal{ID: otherID.String()}, "04DEADBEEF", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.BindCard(ctx, member, "", &otherID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member unbind %v", err)
	}
	got, err := svc.BindCard(ctx, yk, "", &otherID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != otherID || got.StudentCardUID != "" {
		t.Fatalf("privileged unbind %+v", got)
	}
}

func TestService_BindSelfOnly(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	svc := newSvc(t, store, time.Minute)
	ctx := context.Background()
	a := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	b := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	seedUser(t, store, a, "a@example.com", "A", "A")
	seedUser(t, store, b, "b@example.com", "B", "B")
	yk := authz.Principal{ID: a.String(), Groups: []string{"/UYELER/YK"}}
	if _, err := svc.BindCard(ctx, yk, "04FEEDFACE", &b); !errors.Is(err, ErrForbidden) {
		t.Fatalf("privileged bind other %v", err)
	}
}

func TestService_EnsureKeepsBoundUID(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	svc := newSvc(t, store, time.Minute)
	id := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	seedUser(t, store, id, "ada@example.com", "Ada", "Lovelace")
	if _, err := svc.BindCard(context.Background(), authz.Principal{ID: id.String()}, "0411223344", nil); err != nil {
		t.Fatal(err)
	}
	again, _, err := user.NewService(store).Ensure(context.Background(), id, user.Profile{Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"})
	if err != nil {
		t.Fatal(err)
	}
	if again.StudentCardUID != "0411223344" {
		t.Fatalf("ensure wiped uid %+v", again)
	}
}

func TestService_MintIsSignedNotForgeablePlaintext(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	svc := newSvc(t, store, time.Minute)
	id := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	u := seedUser(t, store, id, "ada@example.com", "Ada", "Lovelace")
	tok, err := svc.Mint(context.Background(), authz.Principal{ID: id.String()})
	if err != nil {
		t.Fatal(err)
	}
	if tok.Value == "" || strings.HasPrefix(tok.Value, "SKYPASS:") {
		t.Fatalf("forgeable token %q", tok.Value)
	}
	plain := "SKYPASS:" + u.SkyNumber + ":Ada Lovelace"
	if tok.Value == plain {
		t.Fatal("minted forgeable plaintext")
	}
	parts := strings.Split(tok.Value, ".")
	if len(parts) != 3 {
		t.Fatalf("parts %d", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if claims["skyNumber"] != u.SkyNumber {
		t.Fatalf("claims %+v", claims)
	}
	if _, ok := claims["profilePictureUrl"]; ok {
		t.Fatalf("photo on pass %+v", claims)
	}
	if claims["name"] != "Ada Lovelace" {
		t.Fatalf("name %+v", claims)
	}
	if strings.Contains(string(payload), "SKYPASS:") {
		t.Fatalf("payload %s", payload)
	}
}

func TestService_VerifyRejectsExpiredAndUnsigned(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	key := testKey(t)
	signer := NewSigner(key, time.Minute)
	frozen := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	signer.Now = func() time.Time { return frozen }
	svc := NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()), signer)
	id := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	u := seedUser(t, store, id, "ada@example.com", "Ada", "Lovelace")
	staff := authz.Principal{ID: id.String(), Groups: []string{"/UYELER/YK"}}
	member := authz.Principal{ID: id.String(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}

	tok, err := svc.Mint(context.Background(), authz.Principal{ID: id.String()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Verify(context.Background(), member, tok.Value); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member verify %v", err)
	}
	got, err := svc.Verify(context.Background(), staff, tok.Value)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != id || got.SkyNumber != u.SkyNumber {
		t.Fatalf("verify %+v", got)
	}

	signer.Now = func() time.Time { return frozen.Add(2 * time.Minute) }
	if _, err := svc.Verify(context.Background(), staff, tok.Value); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired %v", err)
	}

	unsigned := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"sub": id.String(), "iss": Issuer, "aud": Audience, "skyNumber": u.SkyNumber,
		"exp": frozen.Add(time.Hour).Unix(),
	})
	raw, err := unsigned.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Verify(context.Background(), staff, raw); err == nil || errors.Is(err, ErrForbidden) {
		t.Fatalf("unsigned %v", err)
	}

	plain := "SKYPASS:" + u.SkyNumber + ":Ada Lovelace"
	if _, err := svc.Verify(context.Background(), staff, plain); !errors.Is(err, ErrInvalid) {
		t.Fatalf("plaintext %v", err)
	}
}

func TestService_LookupForbiddenForMember(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	svc := newSvc(t, store, time.Minute)
	id := uuid.MustParse("99999999-9999-9999-9999-999999999999")
	seedUser(t, store, id, "ada@example.com", "Ada", "Lovelace")
	if _, err := svc.BindCard(context.Background(), authz.Principal{ID: id.String()}, "04ABCDABCD", nil); err != nil {
		t.Fatal(err)
	}
	member := authz.Principal{ID: id.String(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	if _, err := svc.Lookup(context.Background(), member, "04ABCDABCD"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member lookup %v", err)
	}
}

func TestService_JWKSHasRSAPublicKey(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	svc := newSvc(t, store, time.Minute)
	doc := svc.JWKS()
	if len(doc.Keys) != 1 || doc.Keys[0].Kty != "RSA" || doc.Keys[0].N == "" || doc.Keys[0].E == "" {
		t.Fatalf("jwks %+v", doc)
	}
}

func TestService_HolderFromTokenOrUIDWithoutStaffGate(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	svc := newSvc(t, store, time.Minute)
	id := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaa01")
	seedUser(t, store, id, "ada@example.com", "Ada", "Lovelace")
	if _, err := svc.BindCard(context.Background(), authz.Principal{ID: id.String()}, "04AABBCCDD", nil); err != nil {
		t.Fatal(err)
	}
	tok, err := svc.Mint(context.Background(), authz.Principal{ID: id.String()})
	if err != nil {
		t.Fatal(err)
	}
	fromToken, err := svc.HolderFrom(context.Background(), tok.Value, "")
	if err != nil || fromToken.ID != id {
		t.Fatalf("token %v %+v", err, fromToken)
	}
	fromUID, err := svc.HolderFrom(context.Background(), "", "04aa:bb:cc:dd")
	if err != nil || fromUID.ID != id {
		t.Fatalf("uid %v %+v", err, fromUID)
	}
	if _, err := svc.HolderFrom(context.Background(), "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty %v", err)
	}
	plain := "SKYPASS:SKY-1:Ada Lovelace"
	if _, err := svc.HolderFrom(context.Background(), plain, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("plaintext %v", err)
	}
}

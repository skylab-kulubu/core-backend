package skypass

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func testLegacyRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func testECKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
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
	return NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()), NewSigner(testECKey(t), ttl))
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

func TestService_RejectsPreviouslyMintedPassAfterDeletionRequest(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := user.NewMemoryStore()
	svc := newSvc(t, store, time.Minute)
	id := uuid.MustParse("12121212-3434-5656-7878-909090909090")
	seedUser(t, store, id, "pass@example.com", "Sky", "Pass")
	token, err := svc.Mint(ctx, authz.Principal{ID: id.String()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestDeletion(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	staff := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	if _, err := svc.Verify(ctx, staff, token.Value); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old pass verification error = %v, want ErrNotFound", err)
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
	if claims["sub"] != u.ID.String() {
		t.Fatalf("claims %+v", claims)
	}
	if _, ok := claims["profilePictureUrl"]; ok {
		t.Fatalf("photo on pass %+v", claims)
	}
	if _, ok := claims["name"]; ok {
		t.Fatalf("name on pass %+v", claims)
	}
	if _, ok := claims["skyNumber"]; ok {
		t.Fatalf("sky number on pass %+v", claims)
	}
	if strings.Contains(string(payload), "SKYPASS:") {
		t.Fatalf("payload %s", payload)
	}
}

func TestService_MintUsesCompactES256Claims(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	signer := NewSigner(testECKey(t), time.Minute)
	svc := NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()), signer)
	id := uuid.MustParse("77777777-7777-7777-7777-777777777778")
	seedUser(t, store, id, "grace@example.com", "Grace", "Hopper")

	tok, err := svc.Mint(context.Background(), authz.Principal{ID: id.String()})
	if err != nil {
		t.Fatal(err)
	}
	if len(tok.Value) > 260 {
		t.Fatalf("token length = %d, want at most 260", len(tok.Value))
	}
	parsed, _, err := jwt.NewParser().ParseUnverified(tok.Value, jwt.MapClaims{})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Method.Alg() != "ES256" || parsed.Header["kid"] != "sp-e1" {
		t.Fatalf("header = %+v", parsed.Header)
	}
	claims := parsed.Claims.(jwt.MapClaims)
	if claims["sub"] != id.String() || claims["exp"] == nil {
		t.Fatalf("claims = %+v", claims)
	}
	for _, redundant := range []string{"name", "skyNumber", "iss", "aud", "iat"} {
		if _, ok := claims[redundant]; ok {
			t.Fatalf("redundant claim %q in %+v", redundant, claims)
		}
	}
}

func TestParseSigningKeyDerivesStableP256KeyFromLegacyRSA(t *testing.T) {
	t.Parallel()
	legacy := testLegacyRSAKey(t)
	raw := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(legacy)})

	first, err := ParseSigningKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ParseSigningKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	if first.Curve != elliptic.P256() || first.D.Cmp(second.D) != 0 {
		t.Fatalf("derived keys differ or use the wrong curve")
	}
}

func TestService_VerifyRejectsExpiredAndUnsigned(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	key := testECKey(t)
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
		"sub": id.String(), "exp": frozen.Add(time.Hour).Unix(),
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

func TestService_JWKSHasES256PublicKey(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	signer := NewSigner(testECKey(t), time.Minute)
	svc := NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()), signer)
	doc := svc.JWKS()
	if len(doc.Keys) != 1 || doc.Keys[0].Kty != "EC" || doc.Keys[0].Alg != "ES256" || doc.Keys[0].Crv != "P-256" || doc.Keys[0].X == "" || doc.Keys[0].Y == "" {
		t.Fatalf("jwks %+v", doc)
	}
	xBytes, err := base64.RawURLEncoding.DecodeString(doc.Keys[0].X)
	if err != nil {
		t.Fatal(err)
	}
	yBytes, err := base64.RawURLEncoding.DecodeString(doc.Keys[0].Y)
	if err != nil {
		t.Fatal(err)
	}
	public := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(xBytes), Y: new(big.Int).SetBytes(yBytes)}
	if !public.Curve.IsOnCurve(public.X, public.Y) {
		t.Fatal("JWKS coordinates are not on P-256")
	}
	tok, err := signer.Mint(user.User{ID: uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaa02")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jwt.Parse(tok.Value, func(*jwt.Token) (any, error) { return public, nil }, jwt.WithValidMethods([]string{"ES256"})); err != nil {
		t.Fatalf("published key did not verify minted token: %v", err)
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

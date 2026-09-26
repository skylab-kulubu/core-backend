package transit_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/transit"
	"github.com/skylab-kulubu/core-backend/internal/transit/transittest"
)

func dataKey() []byte {
	return bytes.Repeat([]byte{0x5a}, 32)
}

func TestWrappedKeyUnwrapsAndNamesItsKeyVersion(t *testing.T) {
	t.Parallel()
	bao := transittest.NewServer(t)
	client := transit.New(bao.Config())
	ctx := context.Background()

	wrapped, version, err := client.WrapKey(ctx, dataKey())
	if err != nil {
		t.Fatal(err)
	}
	if version != 1 || !bytes.HasPrefix([]byte(wrapped), []byte("vault:v1:")) {
		t.Fatalf("wrapped %q as version %d", wrapped, version)
	}
	got, err := client.UnwrapKey(ctx, wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, dataKey()) {
		t.Fatal("unwrapped a different key")
	}
	if bao.Logins() != 1 {
		t.Fatalf("%d AppRole logins for two calls, want the token reused", bao.Logins())
	}
}

func TestKeyVersionIsReadFromTheCiphertext(t *testing.T) {
	t.Parallel()
	for ciphertext, want := range map[string]int{
		"vault:v1:AAAA":  1,
		"vault:v12:AAAA": 12,
	} {
		if got, err := transit.KeyVersion(ciphertext); err != nil || got != want {
			t.Errorf("%q: version %d, err %v; want %d", ciphertext, got, err, want)
		}
	}
	for _, ciphertext := range []string{"", "vault:v0:AAAA", "vault:vx:AAAA", "vault:v1", "v1:AAAA", "vault:v-1:AAAA"} {
		if _, err := transit.KeyVersion(ciphertext); err == nil {
			t.Errorf("%q: read a version", ciphertext)
		}
	}
}

// eventually waits a little for a condition the client reaches in the
// background.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTokenIsRenewedPastHalfItsLease(t *testing.T) {
	t.Parallel()
	bao := transittest.NewServer(t)
	client := transit.New(bao.Config())
	ctx := context.Background()
	if _, _, err := client.WrapKey(ctx, dataKey()); err != nil {
		t.Fatal(err)
	}

	bao.Clock.Advance(bao.TTL/2 + time.Minute)
	if _, _, err := client.WrapKey(ctx, dataKey()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the token is renewed", func() bool { return bao.Renewals() == 1 })
	if bao.Logins() != 1 {
		t.Fatalf("%d logins, want the token renewed", bao.Logins())
	}
}

// A token with lease left keeps working while OpenBao refuses to renew it or
// to log in again: it is only replaced by a new one.
func TestTokenIsKeptUntilANewOneArrives(t *testing.T) {
	t.Parallel()
	bao := transittest.NewServer(t)
	client := transit.New(bao.Config())
	ctx := context.Background()
	if _, _, err := client.WrapKey(ctx, dataKey()); err != nil {
		t.Fatal(err)
	}

	bao.RefuseRenewalsAndLogins()
	bao.Clock.Advance(bao.TTL/2 + time.Minute)
	for range 3 {
		if _, _, err := client.WrapKey(ctx, dataKey()); err != nil {
			t.Fatalf("with lease left: %v", err)
		}
	}
}

func TestClientLogsInAgainWhenTheTokenReachesItsMaximum(t *testing.T) {
	t.Parallel()
	bao := transittest.NewServer(t)
	client := transit.New(bao.Config())
	ctx := context.Background()

	for elapsed := time.Duration(0); elapsed <= bao.MaxTTL+bao.TTL; elapsed += bao.TTL / 3 {
		if _, _, err := client.WrapKey(ctx, dataKey()); err != nil {
			t.Fatalf("after %s: %v", elapsed, err)
		}
		bao.Clock.Advance(bao.TTL / 3)
	}
	if bao.Logins() < 2 {
		t.Fatalf("%d logins past the token's maximum lifetime", bao.Logins())
	}
}

func TestClientLogsInAgainWhenTheTokenIsRefused(t *testing.T) {
	t.Parallel()
	bao := transittest.NewServer(t)
	client := transit.New(bao.Config())
	ctx := context.Background()
	wrapped, _, err := client.WrapKey(ctx, dataKey())
	if err != nil {
		t.Fatal(err)
	}

	bao.RevokeTokens()
	if _, err := client.UnwrapKey(ctx, wrapped); err != nil {
		t.Fatalf("after the token was revoked: %v", err)
	}
	if bao.Logins() != 2 {
		t.Fatalf("%d logins, want one more after the refusal", bao.Logins())
	}
}

func TestOlderKeyVersionsStillUnwrapAfterRotation(t *testing.T) {
	t.Parallel()
	bao := transittest.NewServer(t)
	client := transit.New(bao.Config())
	ctx := context.Background()
	before, _, err := client.WrapKey(ctx, dataKey())
	if err != nil {
		t.Fatal(err)
	}

	bao.Rotate()
	after, version, err := client.WrapKey(ctx, dataKey())
	if err != nil || version != 2 {
		t.Fatalf("after rotation: version %d, err %v", version, err)
	}
	for _, wrapped := range []string{before, after} {
		got, err := client.UnwrapKey(ctx, wrapped)
		if err != nil || !bytes.Equal(got, dataKey()) {
			t.Fatalf("%.9s: err %v", wrapped, err)
		}
	}
}

func TestUnknownKeyVersionIsRejectedNotUnavailable(t *testing.T) {
	t.Parallel()
	bao := transittest.NewServer(t)
	client := transit.New(bao.Config())
	wrapped, _, err := client.WrapKey(context.Background(), dataKey())
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.UnwrapKey(context.Background(), "vault:v7:"+wrapped[len("vault:v1:"):])
	if !errors.Is(err, transit.ErrRejected) || errors.Is(err, transit.ErrUnavailable) {
		t.Fatalf("err = %v, want %v", err, transit.ErrRejected)
	}
}

func TestOpenBaoDownIsUnavailable(t *testing.T) {
	t.Parallel()
	for name, breakIt := range map[string]func(*transittest.Server){
		"unreachable": func(s *transittest.Server) { s.Close() },
		"sealed":      func(s *transittest.Server) { s.Seal() },
	} {
		bao := transittest.NewServer(t)
		client := transit.New(bao.Config())
		breakIt(bao)
		if _, _, err := client.WrapKey(context.Background(), dataKey()); !errors.Is(err, transit.ErrUnavailable) {
			t.Errorf("%s: err = %v, want %v", name, err, transit.ErrUnavailable)
		}
	}
}

// A Transit mount without the configured key is a configuration mistake,
// not a bad ciphertext. (Encrypt without the key is a permission refusal
// right after a fresh login: it would create the key.)
func TestMissingTransitKeyIsMisconfigured(t *testing.T) {
	t.Parallel()
	bao := transittest.NewServer(t)
	config := bao.Config()
	config.Key = "other"
	client := transit.New(config)

	_, err := client.UnwrapKey(context.Background(), "vault:v1:AAAA")
	if !errors.Is(err, transit.ErrMisconfigured) || errors.Is(err, transit.ErrRejected) {
		t.Fatalf("decrypt: err = %v, want %v", err, transit.ErrMisconfigured)
	}
	if !strings.Contains(err.Error(), "transit/test") || !strings.Contains(err.Error(), "other") {
		t.Fatalf("the error does not name the mount and key: %v", err)
	}
	if _, _, err := transit.New(config).WrapKey(context.Background(), dataKey()); !errors.Is(err, transit.ErrMisconfigured) {
		t.Fatalf("encrypt: err = %v, want %v", err, transit.ErrMisconfigured)
	}
}

// blackhole accepts connections and never answers.
func blackhole(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			conn.Close()
		}
	})
	return "http://" + listener.Addr().String()
}

// Callers waiting for a login share it: with OpenBao blackholed, fifty at
// once all fail within about one request timeout, not one after another.
func TestBlackholedOpenBaoFailsEveryCallerWithinOneTimeout(t *testing.T) {
	t.Parallel()
	timeout := 300 * time.Millisecond
	client := transit.New(transit.Config{Addr: blackhole(t), Mount: "transit/test", Key: "media", RoleID: "r", SecretID: "s", Timeout: timeout})

	started := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := client.WrapKey(context.Background(), dataKey())
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	if elapsed := time.Since(started); elapsed > 3*timeout {
		t.Fatalf("fifty callers took %s with a %s timeout", elapsed, timeout)
	}
	for err := range errs {
		if !errors.Is(err, transit.ErrUnavailable) {
			t.Fatalf("err = %v, want %v", err, transit.ErrUnavailable)
		}
	}
}

// A caller that stops waiting is not held by a login in progress.
func TestWaitingForALoginFollowsTheCallersContext(t *testing.T) {
	t.Parallel()
	client := transit.New(transit.Config{Addr: blackhole(t), Mount: "transit/test", Key: "media", RoleID: "r", SecretID: "s", Timeout: 2 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, _, err := client.WrapKey(ctx, dataKey())
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, transit.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("waited %s past the caller's deadline", elapsed)
	}
}

// A failed login is remembered for a few seconds, so callers in that window
// fail at once instead of each trying again.
func TestFailedLoginIsNotRetriedAtOnce(t *testing.T) {
	t.Parallel()
	bao := transittest.NewServer(t)
	client := transit.New(bao.Config())
	bao.Seal()
	if _, _, err := client.WrapKey(context.Background(), dataKey()); !errors.Is(err, transit.ErrUnavailable) {
		t.Fatalf("sealed: err = %v", err)
	}

	bao.Unseal()
	if _, _, err := client.WrapKey(context.Background(), dataKey()); !errors.Is(err, transit.ErrUnavailable) {
		t.Fatalf("right after the failure: err = %v", err)
	}
	bao.Clock.Advance(10 * time.Second)
	if _, _, err := client.WrapKey(context.Background(), dataKey()); err != nil {
		t.Fatalf("after the failure window: %v", err)
	}
}

// An OpenBao redirect is not followed: the token would go wherever it points.
func TestRedirectIsNotFollowed(t *testing.T) {
	t.Parallel()
	var followed atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed.Add(1) }))
	t.Cleanup(target.Close)
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirecting.Close)

	client := transit.New(transit.Config{Addr: redirecting.URL, Mount: "transit/test", Key: "media", RoleID: "r", SecretID: "s"})
	if _, _, err := client.WrapKey(context.Background(), dataKey()); !errors.Is(err, transit.ErrUnavailable) {
		t.Fatalf("err = %v, want %v", err, transit.ErrUnavailable)
	}
	if followed.Load() != 0 {
		t.Fatal("the redirect was followed")
	}
}

func TestWrongAppRoleCredentialsAreDenied(t *testing.T) {
	t.Parallel()
	bao := transittest.NewServer(t)
	config := bao.Config()
	config.SecretID = "not-the-secret-id"

	_, _, err := transit.New(config).WrapKey(context.Background(), dataKey())
	if !errors.Is(err, transit.ErrDenied) {
		t.Fatalf("err = %v, want %v", err, transit.ErrDenied)
	}
}

// A renewal that fails is held back like a failed login: an OpenBao answering
// 503 sees one renewal and one login, not a pair on every call.
func TestFailingRenewalIsNotRetriedOnEveryCall(t *testing.T) {
	t.Parallel()
	bao := transittest.NewServer(t)
	client := transit.New(bao.Config())
	ctx := context.Background()
	if _, _, err := client.WrapKey(ctx, dataKey()); err != nil {
		t.Fatal(err)
	}
	before := bao.AuthAttempts()

	bao.FailAuth()
	bao.Clock.Advance(bao.TTL/2 + time.Minute)
	for range 10 {
		if _, _, err := client.WrapKey(ctx, dataKey()); err != nil {
			t.Fatalf("with lease left: %v", err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	eventually(t, "the renewal and its login are tried", func() bool { return bao.AuthAttempts()-before >= 2 })
	time.Sleep(20 * time.Millisecond)
	if got := bao.AuthAttempts() - before; got != 2 {
		t.Fatalf("%d renewals and logins tried, want one of each", got)
	}
}

// A token refused right after a fresh login is core's policy, not the token:
// OpenBao is misconfigured, and core does not log in again until the hold
// ends.
func TestPolicyRefusalAfterAFreshLoginIsMisconfigured(t *testing.T) {
	t.Parallel()
	bao := transittest.NewServer(t)
	client := transit.New(bao.Config())
	bao.DenyTransit()

	for range 5 {
		if _, _, err := client.WrapKey(context.Background(), dataKey()); !errors.Is(err, transit.ErrMisconfigured) {
			t.Fatalf("err = %v, want %v", err, transit.ErrMisconfigured)
		}
	}
	if bao.Logins() != 1 {
		t.Fatalf("%d logins, want one until the hold ends", bao.Logins())
	}
	bao.Clock.Advance(10 * time.Second)
	_, _, _ = client.WrapKey(context.Background(), dataKey())
	if bao.Logins() != 2 {
		t.Fatalf("%d logins after the hold, want one more", bao.Logins())
	}
}

// A token replaced by a new login is revoked, as far as OpenBao lets it.
func TestReplacedTokenIsRevoked(t *testing.T) {
	t.Parallel()
	bao := transittest.NewServer(t)
	client := transit.New(bao.Config())
	ctx := context.Background()
	if _, _, err := client.WrapKey(ctx, dataKey()); err != nil {
		t.Fatal(err)
	}

	bao.RefuseRenewals()
	bao.Clock.Advance(bao.TTL/2 + time.Minute)
	if _, _, err := client.WrapKey(ctx, dataKey()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the old token is revoked", func() bool { return bao.Logins() == 2 && bao.Revocations() == 1 })
}

// A short lease does not make every call log in again.
func TestShortLeaseIsKeptInProportion(t *testing.T) {
	t.Parallel()
	bao := transittest.NewServer(t)
	bao.TTL, bao.MaxTTL = 20*time.Second, time.Hour
	client := transit.New(bao.Config())
	for range 5 {
		if _, _, err := client.WrapKey(context.Background(), dataKey()); err != nil {
			t.Fatal(err)
		}
		bao.Clock.Advance(time.Second)
	}
	if bao.Logins() != 1 {
		t.Fatalf("%d logins within a 20-second lease", bao.Logins())
	}
}

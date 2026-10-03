package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Nerzal/gocloak/v13"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// slowKeycloak is a Keycloak that can be made to hang. A request to a gated
// path waits until its gate is closed or the client gives up, so a test never
// leaves a handler behind for Server.Close to wait on.
type slowKeycloak struct {
	// tokenGate holds every token request until it is closed. Nil answers at once.
	tokenGate chan struct{}
	// apiGate holds every admin request the same way.
	apiGate chan struct{}
	// hangFirstTokenRequests holds the first n token requests until the client
	// gives up; later ones are answered at once.
	hangFirstTokenRequests int32
	// tokenDelay is how long a token request takes to answer; tokenExpiresIn
	// is the lifetime it reports in seconds (zero: 300).
	tokenDelay     time.Duration
	tokenExpiresIn int

	tokenRequests atomic.Int32
	server        *httptest.Server
}

func newSlowKeycloak(t *testing.T, k *slowKeycloak) *slowKeycloak {
	t.Helper()
	hold := func(w http.ResponseWriter, r *http.Request, gate chan struct{}) bool {
		select {
		case <-r.Context().Done():
			return false
		case <-gate:
			return true
		}
	}
	k.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The server notices a client that went away only once the request
		// body has been read.
		_, _ = io.Copy(io.Discard, r.Body)
		if strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token") {
			n := k.tokenRequests.Add(1)
			if n <= k.hangFirstTokenRequests {
				<-r.Context().Done()
				return
			}
			if k.tokenGate != nil && !hold(w, r, k.tokenGate) {
				return
			}
			time.Sleep(k.tokenDelay)
			expiresIn := k.tokenExpiresIn
			if expiresIn == 0 {
				expiresIn = 300
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": expiresIn, "token_type": "Bearer"})
			return
		}
		if k.apiGate != nil && !hold(w, r, k.apiGate) {
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/groups"):
			_ = json.NewEncoder(w).Encode([]any{})
		case strings.Contains(r.URL.Path, "/groups/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "g1", "name": "G", "path": "/G"})
		default:
			_ = json.NewEncoder(w).Encode([]any{})
		}
	}))
	t.Cleanup(func() {
		// A client with no timeout would keep its handler, and so Close,
		// waiting for ever.
		k.server.CloseClientConnections()
		k.server.Close()
	})
	return k
}

func (k *slowKeycloak) directory(timeout time.Duration) *identity.Keycloak {
	return identity.NewKeycloak(identity.KeycloakConfig{
		URL: k.server.URL, Realm: "e-skylab", ClientID: "core", ClientSecret: "secret", Timeout: timeout,
	})
}

// within runs fn and fails the test if it has not returned in d: a hung
// Keycloak call is the failure these tests look for, and without the bound it
// would hang the test binary instead of failing it.
func within(t *testing.T, d time.Duration, what string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("%s did not return within %s", what, d)
		return nil
	}
}

// endedAtItsDeadline reports whether err says a deadline passed. gocloak turns
// the errors of its own requests into strings, so for those the text is all
// there is to look at.
func endedAtItsDeadline(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || (err != nil && strings.Contains(err.Error(), "deadline exceeded"))
}

func waitForTokenRequests(t *testing.T, k *slowKeycloak, n int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for k.tokenRequests.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("Keycloak saw %d token requests, want %d", k.tokenRequests.Load(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Every call ends at the configured timeout when Keycloak stops answering, at
// the token request or at the admin request that follows it.
func TestKeycloakCallsEndAtTheConfiguredTimeoutWhenKeycloakStopsAnswering(t *testing.T) {
	t.Parallel()

	calls := map[string]func(context.Context, *identity.Keycloak) error{
		"ListGroups (gocloak)": func(ctx context.Context, d *identity.Keycloak) error { _, err := d.ListGroups(ctx); return err },
		"SearchUsers": func(ctx context.Context, d *identity.Keycloak) error {
			_, err := d.SearchUsers(ctx, "ada", 5)
			return err
		},
		"ListUsers":     func(ctx context.Context, d *identity.Keycloak) error { _, err := d.ListUsers(ctx); return err },
		"YTUAttributes": func(ctx context.Context, d *identity.Keycloak) error { _, err := d.YTUAttributes(ctx); return err },
	}
	hangs := map[string]func() *slowKeycloak{
		"token request": func() *slowKeycloak { return &slowKeycloak{tokenGate: make(chan struct{})} },
		"admin request": func() *slowKeycloak { return &slowKeycloak{apiGate: make(chan struct{})} },
	}
	for callName, call := range calls {
		for hangName, hang := range hangs {
			t.Run(callName+" with a hung "+hangName, func(t *testing.T) {
				t.Parallel()
				fake := newSlowKeycloak(t, hang())
				dir := fake.directory(200 * time.Millisecond)
				err := within(t, 5*time.Second, callName, func() error { return call(context.Background(), dir) })
				if !endedAtItsDeadline(err) {
					t.Fatalf("got %v, want a deadline error", err)
				}
			})
		}
	}
}

// One hung token request must not hold up every other Keycloak call in core:
// a caller waits for the token only as long as its own context allows.
func TestKeycloakCallerWaitingForAHungTokenRequestGivesUpWithItsOwnContext(t *testing.T) {
	t.Parallel()

	fake := newSlowKeycloak(t, &slowKeycloak{tokenGate: make(chan struct{})})
	dir := fake.directory(time.Minute)

	first := make(chan error, 1)
	go func() { _, err := dir.ListGroups(context.Background()); first <- err }()
	waitForTokenRequests(t, fake, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	err := within(t, 3*time.Second, "the second caller", func() error { _, err := dir.ListGroups(ctx); return err })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second caller got %v, want its own deadline", err)
	}
	if got := fake.tokenRequests.Load(); got != 1 {
		t.Fatalf("%d token requests while one was in flight, want 1", got)
	}

	close(fake.tokenGate)
	if err := within(t, 5*time.Second, "the first caller", func() error { return <-first }); err != nil {
		t.Fatalf("first caller: %v", err)
	}
}

// Whoever asked first for the token does not decide, by cancelling, whether
// the others get it.
func TestKeycloakCancellingTheCallerThatStartedTheTokenRequestDoesNotFailTheOthers(t *testing.T) {
	t.Parallel()

	fake := newSlowKeycloak(t, &slowKeycloak{tokenGate: make(chan struct{})})
	dir := fake.directory(time.Minute)

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := dir.ListGroups(firstCtx); first <- err }()
	waitForTokenRequests(t, fake, 1)

	second := make(chan error, 1)
	go func() { _, err := dir.ListGroups(context.Background()); second <- err }()

	cancelFirst()
	if err := within(t, 3*time.Second, "the cancelled caller", func() error { return <-first }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller got %v, want context.Canceled", err)
	}

	close(fake.tokenGate)
	if err := within(t, 5*time.Second, "the other caller", func() error { return <-second }); err != nil {
		t.Fatalf("other caller: %v", err)
	}
	if got := fake.tokenRequests.Load(); got != 1 {
		t.Fatalf("%d token requests, want the one the first caller started", got)
	}
}

// Callers that arrive together share one token request.
func TestKeycloakConcurrentCallersShareOneTokenRequest(t *testing.T) {
	t.Parallel()

	fake := newSlowKeycloak(t, &slowKeycloak{tokenGate: make(chan struct{})})
	dir := fake.directory(time.Minute)

	const callers = 8
	var started, finished sync.WaitGroup
	errs := make(chan error, callers)
	started.Add(callers)
	finished.Add(callers)
	for range callers {
		go func() {
			defer finished.Done()
			started.Done()
			_, err := dir.ListGroups(context.Background())
			errs <- err
		}()
	}
	started.Wait()
	waitForTokenRequests(t, fake, 1)
	close(fake.tokenGate)
	finished.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := fake.tokenRequests.Load(); got != 1 {
		t.Fatalf("%d token requests for %d concurrent callers, want 1", got, callers)
	}
}

// A token request that timed out is not remembered: the next call asks again.
func TestKeycloakFailedTokenRequestIsRetriedByTheNextCall(t *testing.T) {
	t.Parallel()

	fake := newSlowKeycloak(t, &slowKeycloak{hangFirstTokenRequests: 1})
	// Long enough that the second call, which Keycloak answers at once, never
	// meets the timeout on a busy machine.
	dir := fake.directory(time.Second)

	err := within(t, 5*time.Second, "the first call", func() error { _, err := dir.ListGroups(context.Background()); return err })
	if !endedAtItsDeadline(err) {
		t.Fatalf("first call got %v, want a deadline error", err)
	}
	if err := within(t, 5*time.Second, "the second call", func() error { _, err := dir.ListGroups(context.Background()); return err }); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := fake.tokenRequests.Load(); got != 2 {
		t.Fatalf("%d token requests, want 2", got)
	}
}

// ListUsers once ignored its context; the caller's cancellation must stop it.
func TestKeycloakListUsersStopsWithItsContext(t *testing.T) {
	t.Parallel()

	fake := newSlowKeycloak(t, &slowKeycloak{apiGate: make(chan struct{})})
	dir := fake.directory(time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	err := within(t, 3*time.Second, "ListUsers", func() error { _, err := dir.ListUsers(ctx); return err })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the context's deadline", err)
	}
}

func TestKeycloakZeroTimeoutMeansTheDefault(t *testing.T) {
	t.Parallel()

	dir := identity.NewKeycloak(identity.KeycloakConfig{URL: "http://127.0.0.1:1", Realm: "e-skylab", ClientID: "core", ClientSecret: "secret"})
	if got := dir.TimeoutForTest(); got != identity.DefaultKeycloakTimeout {
		t.Fatalf("timeout %s, want %s", got, identity.DefaultKeycloakTimeout)
	}
	if identity.DefaultKeycloakTimeout <= 0 {
		t.Fatalf("default timeout %s", identity.DefaultKeycloakTimeout)
	}
}

// The token's life counts from before the request for it was sent: Keycloak
// dates the token when it issues it, so a slow answer has already used up part
// of that life.
func TestKeycloakTokenLifeCountsFromTheStartOfItsRequest(t *testing.T) {
	t.Parallel()

	// 31 seconds of life, less the 30 held back, is one second; the answer
	// takes longer than that.
	fake := newSlowKeycloak(t, &slowKeycloak{tokenDelay: 1100 * time.Millisecond, tokenExpiresIn: 31})
	dir := fake.directory(time.Minute)

	for call := 1; call <= 2; call++ {
		if err := within(t, 10*time.Second, "ListGroups", func() error { _, err := dir.ListGroups(context.Background()); return err }); err != nil {
			t.Fatalf("call %d: %v", call, err)
		}
	}
	if got := fake.tokenRequests.Load(); got != 2 {
		t.Fatalf("%d token requests, want a second one because the first token had run out when it arrived", got)
	}
}

// A panic in the shared token request must reach the callers as an error: the
// goroutine singleflight runs it in would otherwise end the whole process.
func TestKeycloakPanicInTheTokenRequestIsAnErrorForTheCallers(t *testing.T) {
	t.Parallel()

	fake := newSlowKeycloak(t, &slowKeycloak{})
	dir := fake.directory(time.Minute)
	attempts := 0
	dir.SetLoginForTest(func(context.Context) (*gocloak.JWT, error) {
		attempts++
		if attempts == 1 {
			panic("token decoder blew up")
		}
		return &gocloak.JWT{AccessToken: "tok", ExpiresIn: 300}, nil
	})

	err := within(t, 5*time.Second, "the first call", func() error { _, err := dir.ListGroups(context.Background()); return err })
	if err == nil || !strings.Contains(err.Error(), "token decoder blew up") {
		t.Fatalf("first call got %v, want the panic as an error", err)
	}
	if err := within(t, 5*time.Second, "the second call", func() error { _, err := dir.ListGroups(context.Background()); return err }); err != nil {
		t.Fatalf("second call: %v", err)
	}
}

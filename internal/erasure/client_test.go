package erasure_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/erasure"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

var (
	testRequestID = uuid.MustParse("0b6f2a3e-1111-4222-8333-944444444444")
	testSubjectID = uuid.MustParse("3f1c9d70-5555-4666-8777-988888888888")
	testAddresses = []string{"Ada.Lovelace@std.yildiz.edu.tr", " ada@example.com ", "ada@example.com", ""}
	wantAddresses = []string{"ada.lovelace@std.yildiz.edu.tr", "ada@example.com"}
	testNow       = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
)

type recordedCall struct {
	method string
	path   string
	header http.Header
	body   []byte
}

type fakeService struct {
	mu      sync.Mutex
	server  *httptest.Server
	calls   []recordedCall
	respond func(w http.ResponseWriter, r *http.Request)
}

func newFakeService(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) *fakeService {
	t.Helper()
	service := &fakeService{respond: respond}
	service.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		service.mu.Lock()
		service.calls = append(service.calls, recordedCall{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body})
		respond := service.respond
		service.mu.Unlock()
		respond(w, r)
	}))
	t.Cleanup(service.server.Close)
	return service
}

type staticTokens struct {
	mu          sync.Mutex
	token       string
	err         error
	invalidated int
}

func (s *staticTokens) Token(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token, s.err
}

func (s *staticTokens) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidated++
}

func completed(counts string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"request_id":"` + testRequestID.String() + `","status":"completed","completed_at":"2026-09-25T12:00:00Z","counts":` + counts + `}`))
	}
}

func status(code int, headers map[string]string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		for key, value := range headers {
			w.Header().Set(key, value)
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(code)
		// A misbehaving service writes the addresses back; none may surface.
		_, _ = w.Write([]byte(`{"type":"about:blank","code":"x","detail":"ada@example.com ` + testSubjectID.String() + `"}`))
	}
}

func newClient(service *fakeService, tokens erasure.TokenSource) *erasure.Client {
	return &erasure.Client{
		Service: erasure.Registry()[1],
		BaseURL: service.server.URL,
		Tokens:  tokens,
		Now:     func() time.Time { return testNow },
	}
}

func command() erasure.Command {
	return erasure.Command{RequestID: testRequestID, SubjectID: testSubjectID, Emails: testAddresses}
}

func assertNoPII(t *testing.T, text string) {
	t.Helper()
	lowered := strings.ToLower(text)
	for _, value := range append(append([]string{}, wantAddresses...), testSubjectID.String(), "ada") {
		if strings.Contains(lowered, strings.ToLower(value)) {
			t.Fatalf("%q carries personal data %q", text, value)
		}
	}
}

func TestClientSendsThePUTContractAndReturnsCounts(t *testing.T) {
	t.Parallel()

	service := newFakeService(t, completed(`{"collection_items_updated":3,"drafts_deleted":0}`))
	result, err := newClient(service, &staticTokens{token: "token-a"}).Erase(context.Background(), command())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Counts) != 2 || result.Counts["collection_items_updated"] != 3 || result.Counts["drafts_deleted"] != 0 {
		t.Fatalf("counts = %v", result.Counts)
	}
	if len(service.calls) != 1 {
		t.Fatalf("calls = %d", len(service.calls))
	}
	call := service.calls[0]
	if call.method != http.MethodPut || call.path != "/internal/v1/account-erasures/"+testRequestID.String() {
		t.Fatalf("request line = %s %s", call.method, call.path)
	}
	if call.header.Get("Authorization") != "Bearer token-a" || call.header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", call.header)
	}
	if call.header.Get("Idempotency-Key") != "" {
		t.Fatal("request_id is the idempotency key; no header is sent")
	}
	// Services answer 404 to anything that looks like it came through the
	// public ingress.
	for _, name := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded", "X-Real-Ip"} {
		if value := call.header.Get(name); value != "" {
			t.Fatalf("command carries %s: %q", name, value)
		}
	}
	for name, values := range call.header {
		for _, value := range values {
			if name != "Authorization" {
				assertNoPII(t, name+": "+value)
			}
		}
	}
	var body struct {
		RequestID uuid.UUID `json:"request_id"`
		SubjectID uuid.UUID `json:"subject_id"`
		Emails    []string  `json:"emails"`
	}
	decoder := json.NewDecoder(bytes.NewReader(call.body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		t.Fatalf("body %s: %v", call.body, err)
	}
	if body.RequestID != testRequestID || body.SubjectID != testSubjectID {
		t.Fatalf("body ids = %+v", body)
	}
	if strings.Join(body.Emails, ",") != strings.Join(wantAddresses, ",") {
		t.Fatalf("emails = %v, want %v", body.Emails, wantAddresses)
	}
}

func TestClientSendsAnEmptyAddressListAsAnArray(t *testing.T) {
	t.Parallel()

	service := newFakeService(t, completed(`{}`))
	if _, err := newClient(service, &staticTokens{token: "t"}).Erase(context.Background(), erasure.Command{RequestID: testRequestID, SubjectID: testSubjectID}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(service.calls[0].body, []byte(`"emails":[]`)) {
		t.Fatalf("body = %s", service.calls[0].body)
	}
}

func TestClientRefusesAnOversizedAddressSetWithoutNamingIt(t *testing.T) {
	t.Parallel()

	service := newFakeService(t, completed(`{}`))
	client := newClient(service, &staticTokens{token: "t"})
	for _, emails := range [][]string{
		{"a1@example.com", "a2@example.com", "a3@example.com", "a4@example.com"},
		{strings.Repeat("a", 250) + "@example.com"},
	} {
		_, err := client.Erase(context.Background(), erasure.Command{RequestID: testRequestID, SubjectID: testSubjectID, Emails: emails})
		if err == nil {
			t.Fatalf("addresses %d accepted", len(emails))
		}
		if strings.Contains(err.Error(), "example.com") {
			t.Fatalf("error names an address: %v", err)
		}
	}
	if len(service.calls) != 0 {
		t.Fatal("an invalid command reached the service")
	}
}

func TestClientValidatesTheCompletionBody(t *testing.T) {
	t.Parallel()

	var many strings.Builder
	many.WriteByte('{')
	for i := 0; i < 33; i++ {
		if i > 0 {
			many.WriteByte(',')
		}
		many.WriteString(`"k` + string(rune('a'+i%26)) + string(rune('a'+i/26)) + `":1`)
	}
	many.WriteByte('}')
	for name, respond := range map[string]func(http.ResponseWriter, *http.Request){
		"array counts":     completed(`[1]`),
		"null counts":      completed(`null`),
		"string value":     completed(`{"rows":"1"}`),
		"negative value":   completed(`{"rows":-1}`),
		"fraction":         completed(`{"rows":1.5}`),
		"exponent":         completed(`{"rows":1e3}`),
		"overflow":         completed(`{"rows":92233720368547758070}`),
		"33 keys":          completed(many.String()),
		"address as a key": completed(`{"ada@example.com":1}`),
		"camelCase key":    completed(`{"rowsDeleted":1}`),
		"missing counts": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"request_id":"` + testRequestID.String() + `","status":"completed"}`))
		},
		"other request": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"request_id":"` + uuid.NewString() + `","status":"completed","counts":{}}`))
		},
		"not completed": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"request_id":"` + testRequestID.String() + `","status":"in_progress","counts":{}}`))
		},
		"not json": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`ada@example.com`))
		},
	} {
		service := newFakeService(t, respond)
		_, err := newClient(service, &staticTokens{token: "t"}).Erase(context.Background(), command())
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		var deferred interface{ RetryAt() time.Time }
		var permanent interface{ PermanentCode() string }
		if errors.As(err, &deferred) || errors.As(err, &permanent) {
			t.Fatalf("%s: an invalid body is an ordinary retry, got %T", name, err)
		}
		assertNoPII(t, err.Error())
	}

	exactly32 := strings.Builder{}
	exactly32.WriteByte('{')
	for i := 0; i < 32; i++ {
		if i > 0 {
			exactly32.WriteByte(',')
		}
		exactly32.WriteString(`"k` + string(rune('a'+i%26)) + string(rune('a'+i/26)) + `":9223372036854775807`)
	}
	exactly32.WriteByte('}')
	service := newFakeService(t, completed(exactly32.String()))
	result, err := newClient(service, &staticTokens{token: "t"}).Erase(context.Background(), command())
	if err != nil || len(result.Counts) != 32 {
		t.Fatalf("32 keys: counts=%d err=%v", len(result.Counts), err)
	}
}

func TestClientDefersBusyAndUnavailableServicesByTheClampedRetryAfter(t *testing.T) {
	t.Parallel()

	cases := []struct {
		code       int
		retryAfter string
		want       time.Duration
	}{
		{code: http.StatusAccepted, retryAfter: "120", want: 2 * time.Minute},
		{code: http.StatusAccepted, retryAfter: "", want: 5 * time.Minute},
		{code: http.StatusAccepted, retryAfter: "5", want: 30 * time.Second},
		{code: http.StatusTooManyRequests, retryAfter: "3600", want: 15 * time.Minute},
		{code: http.StatusInternalServerError, retryAfter: "", want: 5 * time.Minute},
		{code: http.StatusBadGateway, retryAfter: "soon", want: 5 * time.Minute},
		{code: http.StatusServiceUnavailable, retryAfter: testNow.Add(90 * time.Second).Format(http.TimeFormat), want: 90 * time.Second},
		{code: http.StatusGatewayTimeout, retryAfter: "60", want: time.Minute},
	}
	for _, tc := range cases {
		headers := map[string]string{}
		if tc.retryAfter != "" {
			headers["Retry-After"] = tc.retryAfter
		}
		service := newFakeService(t, status(tc.code, headers))
		_, err := newClient(service, &staticTokens{token: "t"}).Erase(context.Background(), command())
		var deferred interface{ RetryAt() time.Time }
		if !errors.As(err, &deferred) {
			t.Fatalf("%d: not deferred: %v", tc.code, err)
		}
		if got := deferred.RetryAt().Sub(testNow); got != tc.want {
			t.Fatalf("%d Retry-After %q: wait %s, want %s", tc.code, tc.retryAfter, got, tc.want)
		}
		assertNoPII(t, err.Error())
	}
}

func TestClientTreatsTheStatedRejectionsAsPermanent(t *testing.T) {
	t.Parallel()

	for _, code := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict} {
		service := newFakeService(t, status(code, nil))
		_, err := newClient(service, &staticTokens{token: "t"}).Erase(context.Background(), command())
		var permanent interface{ PermanentCode() string }
		if !errors.As(err, &permanent) {
			t.Fatalf("%d: not permanent: %v", code, err)
		}
		if got := permanent.PermanentCode(); got != "erase_cms_rejected_"+strconv.Itoa(code) {
			t.Fatalf("%d: code %q", code, got)
		}
		var deferred interface{ RetryAt() time.Time }
		if errors.As(err, &deferred) {
			t.Fatalf("%d: a rejection must not be deferred", code)
		}
		assertNoPII(t, err.Error())
	}
}

func TestClientDropsTheTokenOnUnauthorizedAndSpendsAnOrdinaryRetry(t *testing.T) {
	t.Parallel()

	service := newFakeService(t, status(http.StatusUnauthorized, map[string]string{"Retry-After": "1"}))
	tokens := &staticTokens{token: "t"}
	_, err := newClient(service, tokens).Erase(context.Background(), command())
	if err == nil {
		t.Fatal("401 accepted")
	}
	var deferred interface{ RetryAt() time.Time }
	var permanent interface{ PermanentCode() string }
	if errors.As(err, &deferred) || errors.As(err, &permanent) {
		t.Fatalf("401 must be an ordinary retry: %T %v", err, err)
	}
	if tokens.invalidated != 1 {
		t.Fatalf("token invalidated %d times", tokens.invalidated)
	}
	assertNoPII(t, err.Error())
}

func TestClientDefersWhenTheTokenEndpointFails(t *testing.T) {
	t.Parallel()

	endpoint := newTokenEndpoint(t)
	endpoint.set(http.StatusUnauthorized, 300)
	service := newFakeService(t, completed(`{}`))
	client := newClient(service, &erasure.ClientCredentials{
		TokenURL: endpoint.server.URL, ClientID: "core-erasure", ClientSecret: "s3cr3t-value-never-printed", Scope: "account-erase-cms",
	})
	_, err := client.Erase(context.Background(), command())
	var deferred interface{ RetryAt() time.Time }
	if !errors.As(err, &deferred) || deferred.RetryAt().Sub(testNow) != 5*time.Minute {
		t.Fatalf("token failure = %T %v", err, err)
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("error leaks the secret: %v", err)
	}
	if len(service.calls) != 0 {
		t.Fatal("service called without a token")
	}
}

func TestClientDefersOnTimeoutAndConnectionFailure(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	slow := newFakeService(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	client := newClient(slow, &staticTokens{token: "t"})
	client.HTTP = &http.Client{Timeout: 50 * time.Millisecond}
	_, err := client.Erase(context.Background(), command())
	var deferred interface{ RetryAt() time.Time }
	if !errors.As(err, &deferred) || deferred.RetryAt().Sub(testNow) != 5*time.Minute {
		t.Fatalf("timeout = %T %v", err, err)
	}
	assertNoPII(t, err.Error())

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedURL := "http://" + listener.Addr().String()
	_ = listener.Close()
	refused := &erasure.Client{Service: erasure.Registry()[0], BaseURL: closedURL, Tokens: &staticTokens{token: "t"}, Now: func() time.Time { return testNow }}
	_, err = refused.Erase(context.Background(), command())
	if !errors.As(err, &deferred) {
		t.Fatalf("connection failure = %T %v", err, err)
	}
	assertNoPII(t, err.Error())
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	elsewhere := newFakeService(t, completed(`{}`))
	service := newFakeService(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.server.URL+r.URL.Path, http.StatusTemporaryRedirect)
	})
	_, err := newClient(service, &staticTokens{token: "t"}).Erase(context.Background(), command())
	if err == nil {
		t.Fatal("redirect followed to completion")
	}
	if len(elsewhere.calls) != 0 {
		t.Fatal("the command body was sent to the redirect target")
	}
}

func TestClientUnexpectedStatusIsAnOrdinaryRetry(t *testing.T) {
	t.Parallel()

	service := newFakeService(t, status(http.StatusTeapot, nil))
	_, err := newClient(service, &staticTokens{token: "t"}).Erase(context.Background(), command())
	var deferred interface{ RetryAt() time.Time }
	var permanent interface{ PermanentCode() string }
	if err == nil || errors.As(err, &deferred) || errors.As(err, &permanent) {
		t.Fatalf("418 = %T %v", err, err)
	}
	if !strings.Contains(err.Error(), "418") || !strings.HasPrefix(err.Error(), string(user.DeletionStepEraseCMS)+": ") {
		t.Fatalf("418 message = %q", err)
	}
}

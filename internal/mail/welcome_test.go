package mail

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

type tokenFailure struct{}

func (tokenFailure) Token(context.Context) (string, error) { return "", errors.New("keycloak down") }

// welcomeServer answers every Welcome post with status and counts the posts.
func welcomeServer(t *testing.T, status func(n int) int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	posts := new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(status(int(posts.Add(1))))
		_, _ = w.Write([]byte(`{"error":"template_not_found"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, posts
}

// The Welcome mail says what became of it, and every outcome but sent leaves
// one log line that names no person.
func TestWelcomeOutcomes(t *testing.T) {
	t.Parallel()
	u := testUser()
	tests := []struct {
		name      string
		mailer    func(t *testing.T) (*SkyMail, *atomic.Int32)
		want      WelcomeOutcome
		wantPosts int
		wantLog   string
	}{
		{name: "sent", want: WelcomeSent, wantPosts: 1, mailer: func(t *testing.T) (*SkyMail, *atomic.Int32) {
			srv, posts := welcomeServer(t, func(int) int { return http.StatusCreated })
			return &SkyMail{BaseURL: srv.URL, TemplateKey: "core.welcome", Tokens: StaticToken("tok"), HTTP: srv.Client()}, posts
		}},
		{name: "refused", want: WelcomeRefused, wantPosts: 1, wantLog: `"status":422,"reason":"upstream_error"`, mailer: func(t *testing.T) (*SkyMail, *atomic.Int32) {
			srv, posts := welcomeServer(t, func(int) int { return http.StatusUnprocessableEntity })
			return &SkyMail{BaseURL: srv.URL, TemplateKey: "core.welcome", Tokens: StaticToken("tok"), HTTP: srv.Client()}, posts
		}},
		{name: "refused 400", want: WelcomeRefused, wantPosts: 1, wantLog: `"status":400,"reason":"upstream_error"`, mailer: func(t *testing.T) (*SkyMail, *atomic.Int32) {
			srv, posts := welcomeServer(t, func(int) int { return http.StatusBadRequest })
			return &SkyMail{BaseURL: srv.URL, TemplateKey: "core.welcome", Tokens: StaticToken("tok"), HTTP: srv.Client()}, posts
		}},
		{name: "deferred 503", want: WelcomeDeferred, wantPosts: 1, wantLog: `"status":503,"reason":"upstream_error"`, mailer: func(t *testing.T) (*SkyMail, *atomic.Int32) {
			srv, posts := welcomeServer(t, func(int) int { return http.StatusServiceUnavailable })
			return &SkyMail{BaseURL: srv.URL, TemplateKey: "core.welcome", Tokens: StaticToken("tok"), HTTP: srv.Client()}, posts
		}},
		{name: "deferred 429", want: WelcomeDeferred, wantPosts: 1, wantLog: `"status":429,"reason":"upstream_error"`, mailer: func(t *testing.T) (*SkyMail, *atomic.Int32) {
			srv, posts := welcomeServer(t, func(int) int { return http.StatusTooManyRequests })
			return &SkyMail{BaseURL: srv.URL, TemplateKey: "core.welcome", Tokens: StaticToken("tok"), HTTP: srv.Client()}, posts
		}},
		{name: "template not seeded", want: WelcomeDeferred, wantPosts: 1, wantLog: `"status":404,"reason":"template_missing"`, mailer: func(t *testing.T) (*SkyMail, *atomic.Int32) {
			srv, posts := welcomeServer(t, func(int) int { return http.StatusNotFound })
			return &SkyMail{BaseURL: srv.URL, TemplateKey: "core.welcome", Tokens: StaticToken("tok"), HTTP: srv.Client()}, posts
		}},
		{name: "unknown key falls back to the id", want: WelcomeSent, wantPosts: 2, wantLog: `"reason":"template_key_missing_fallback_to_id"`, mailer: func(t *testing.T) (*SkyMail, *atomic.Int32) {
			srv, posts := welcomeServer(t, func(n int) int {
				if n == 1 {
					return http.StatusNotFound
				}
				return http.StatusCreated
			})
			return &SkyMail{BaseURL: srv.URL, TemplateKey: "core.welcome", TemplateID: uuid.New(), Tokens: StaticToken("tok"), HTTP: srv.Client()}, posts
		}},
		{name: "no token", want: WelcomeNotSent, wantLog: `"reason":"token_unavailable"`, mailer: func(t *testing.T) (*SkyMail, *atomic.Int32) {
			srv, posts := welcomeServer(t, func(int) int { return http.StatusCreated })
			return &SkyMail{BaseURL: srv.URL, TemplateKey: "core.welcome", Tokens: tokenFailure{}, HTTP: srv.Client()}, posts
		}},
		{name: "no template", want: WelcomeNotSent, mailer: func(t *testing.T) (*SkyMail, *atomic.Int32) {
			srv, posts := welcomeServer(t, func(int) int { return http.StatusCreated })
			return &SkyMail{BaseURL: srv.URL, Tokens: StaticToken("tok"), HTTP: srv.Client()}, posts
		}},
		{name: "unreachable", want: WelcomeNotSent, wantLog: `"reason":"unreachable"`, mailer: func(t *testing.T) (*SkyMail, *atomic.Int32) {
			srv, posts := welcomeServer(t, func(int) int { return http.StatusCreated })
			base := srv.URL
			srv.Close()
			return &SkyMail{BaseURL: base, TemplateKey: "core.welcome", Tokens: StaticToken("tok"), HTTP: &http.Client{Timeout: 2 * time.Second}}, posts
		}},
		{name: "no answer", want: WelcomeUnknown, wantPosts: 1, wantLog: `"reason":"no_answer"`, mailer: func(t *testing.T) (*SkyMail, *atomic.Int32) {
			posts := new(atomic.Int32)
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}))
			t.Cleanup(func() { close(release); srv.CloseClientConnections(); srv.Close() })
			return &SkyMail{BaseURL: srv.URL, TemplateKey: "core.welcome", Tokens: StaticToken("tok"), HTTP: &http.Client{Timeout: 200 * time.Millisecond}}, posts
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mailer, posts := tc.mailer(t)
			var logs bytes.Buffer
			mailer.Logger = log.New(&logs, "", 0)
			if got := mailer.Welcome(t.Context(), u); got != tc.want {
				t.Fatalf("outcome %q, want %q", got, tc.want)
			}
			if int(posts.Load()) != tc.wantPosts {
				t.Fatalf("%d posts, want %d", posts.Load(), tc.wantPosts)
			}
			text := logs.String()
			if tc.wantLog == "" && text != "" {
				t.Fatalf("log %q, want none", text)
			}
			if tc.wantLog != "" && !strings.Contains(text, `"kind":"welcome"`) {
				t.Fatalf("log %q has no kind welcome", text)
			}
			if tc.wantLog != "" && !strings.Contains(text, tc.wantLog) {
				t.Fatalf("log %q, want %s", text, tc.wantLog)
			}
			for _, personal := range []string{u.Email, u.FirstName, u.LastName, u.SkyNumber} {
				if personal != "" && strings.Contains(text, personal) {
					t.Fatalf("log names the person: %q", text)
				}
			}
		})
	}
}

package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestSkyMailWelcomePostsSingleTask(t *testing.T) {
	t.Parallel()
	templateID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	var gotAuth, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"t1"}`))
	}))
	t.Cleanup(srv.Close)

	mailer := &SkyMail{
		BaseURL:    srv.URL,
		TemplateID: templateID,
		Tokens:     StaticToken("tok"),
		HTTP:       srv.Client(),
	}
	mailer.Welcome(t.Context(), user.User{
		Email:     "ada@example.com",
		FirstName: "Ada",
		LastName:  "Lovelace",
		CreatedAt: time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC),
	})
	if gotAuth != "Bearer tok" {
		t.Fatalf("auth %q", gotAuth)
	}
	if gotPath != "/v1/mail_tasks/single" {
		t.Fatalf("path %q", gotPath)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["template_id"] != templateID.String() {
		t.Fatalf("template %v", payload["template_id"])
	}
	if payload["recipient_email"] != "ada@example.com" {
		t.Fatalf("email %v", payload["recipient_email"])
	}
	if payload["recipient_full_name"] != "Ada Lovelace" {
		t.Fatalf("name %v", payload["recipient_full_name"])
	}
}

func TestSkyMailWelcomeNoopsWithoutTemplate(t *testing.T) {
	t.Parallel()
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	t.Cleanup(srv.Close)
	mailer := &SkyMail{BaseURL: srv.URL, Tokens: StaticToken("tok"), HTTP: srv.Client()}
	mailer.Welcome(t.Context(), user.User{Email: "a@b.c"})
	if called {
		t.Fatal("should not post")
	}
}

func TestSkyMailCertificatePostsSingleTask(t *testing.T) {
	t.Parallel()
	templateID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	var gotPath string
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	mailer := &SkyMail{
		BaseURL:               srv.URL,
		CertificateTemplateID: templateID,
		Tokens:                StaticToken("tok"),
		HTTP:                  srv.Client(),
	}
	mailer.Certificate(t.Context(), "ada@example.com", "Ada Lovelace", map[string]string{"EventName": "ARTLAB"})
	if gotPath != "/v1/mail_tasks/single" {
		t.Fatalf("path %q", gotPath)
	}
	if payload["template_id"] != templateID.String() {
		t.Fatalf("template %v", payload["template_id"])
	}
	if payload["recipient_email"] != "ada@example.com" {
		t.Fatalf("email %v", payload["recipient_email"])
	}
}

func TestSkyMailCertificateNoopsWithoutTemplate(t *testing.T) {
	t.Parallel()
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	t.Cleanup(srv.Close)
	mailer := &SkyMail{BaseURL: srv.URL, Tokens: StaticToken("tok"), HTTP: srv.Client()}
	mailer.Certificate(t.Context(), "ada@example.com", "Ada", nil)
	if called {
		t.Fatal("should not post")
	}
}

func TestAPIOriginDefaultsToPublicSkymailAPI(t *testing.T) {
	t.Parallel()
	if got := APIOrigin(""); got != "https://api.yildizskylab.com/api/skymail" {
		t.Fatalf("empty %q", got)
	}
	if got := APIOrigin("  "); got != "https://api.yildizskylab.com/api/skymail" {
		t.Fatalf("blank %q", got)
	}
	if got := APIOrigin("https://mail.example.test/"); got != "https://mail.example.test" {
		t.Fatalf("override %q", got)
	}
}

const (
	testRecipient   = "ada@example.com"
	testAccessToken = "test-access-token-value"
	testSecret      = "test-client-secret-value"
)

var (
	welcomeTemplateID     = uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	certificateTemplateID = uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
)

func captureWarnings() (*log.Logger, *bytes.Buffer) {
	var out bytes.Buffer
	return log.New(&out, "", 0), &out
}

// assertNoMailMaterial fails when a captured line carries anything that must
// never reach a log file: the recipient, a template id or the access token.
func assertNoMailMaterial(t *testing.T, got string) {
	t.Helper()
	for _, material := range []string{
		testRecipient,
		testAccessToken,
		testSecret,
		welcomeTemplateID.String(),
		certificateTemplateID.String(),
	} {
		if strings.Contains(got, material) {
			t.Fatalf("log exposed %q: %s", material, got)
		}
	}
}

func assertContainsAll(t *testing.T, got string, want []string) {
	t.Helper()
	for _, fragment := range want {
		if !strings.Contains(got, fragment) {
			t.Fatalf("log missing %s: %s", fragment, got)
		}
	}
}

func testUser() user.User {
	return user.User{
		Email:     testRecipient,
		FirstName: "Ada",
		LastName:  "Lovelace",
		SkyNumber: "SKY-1",
		CreatedAt: time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC),
	}
}

// oversizedBody puts the recipient and a template id past the diagnostic cap, so
// a truncating read is the only thing keeping them out of the log.
func oversizedBody() string {
	return `{"error":"template_not_found","detail":"` + strings.Repeat("x", 4<<10) + " " + testRecipient + " " + welcomeTemplateID.String() + `"}`
}

type mailSend struct {
	kind  string
	build func(*httptest.Server, *log.Logger) *SkyMail
	call  func(context.Context, *SkyMail)
}

// mailSendsByID exercises both single sends over the template id addressing, so
// one table covers the welcome and the certificate mail alike.
func mailSendsByID() []mailSend {
	return []mailSend{
		{
			kind: "welcome",
			build: func(srv *httptest.Server, logger *log.Logger) *SkyMail {
				return &SkyMail{
					BaseURL:    srv.URL,
					TemplateID: welcomeTemplateID,
					Tokens:     StaticToken(testAccessToken),
					HTTP:       srv.Client(),
					Logger:     logger,
				}
			},
			call: func(ctx context.Context, m *SkyMail) { m.Welcome(ctx, testUser()) },
		},
		{
			kind: "certificate",
			build: func(srv *httptest.Server, logger *log.Logger) *SkyMail {
				return &SkyMail{
					BaseURL:               srv.URL,
					CertificateTemplateID: certificateTemplateID,
					Tokens:                StaticToken(testAccessToken),
					HTTP:                  srv.Client(),
					Logger:                logger,
				}
			},
			call: func(ctx context.Context, m *SkyMail) {
				m.Certificate(ctx, testRecipient, "Ada Lovelace", map[string]string{"EventName": "ARTLAB"})
			},
		},
	}
}

func TestSkyMailSendWarnsOnRefusedStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		status     int
		body       string
		wantWarn   bool
		wantReason string
		wantCode   string
	}{
		{
			name:   "accepted send stays quiet",
			status: http.StatusCreated,
			body:   `{"id":"11111111-1111-1111-1111-111111111111"}`,
		},
		{
			name:       "archived or unknown template",
			status:     http.StatusNotFound,
			body:       `{"error":"template_not_found"}`,
			wantWarn:   true,
			wantReason: "template_missing",
			wantCode:   "template_not_found",
		},
		{
			name:       "upstream failure",
			status:     http.StatusInternalServerError,
			body:       `{"message":"internal_error"}`,
			wantWarn:   true,
			wantReason: "upstream_error",
			wantCode:   "internal_error",
		},
		{
			name:       "malformed json body",
			status:     http.StatusBadGateway,
			body:       `{"error":`,
			wantWarn:   true,
			wantReason: "upstream_error",
		},
		{
			name:       "oversized body is truncated",
			status:     http.StatusServiceUnavailable,
			body:       oversizedBody(),
			wantWarn:   true,
			wantReason: "upstream_error",
		},
		{
			name:       "prose body naming the recipient",
			status:     http.StatusForbidden,
			body:       `{"message":"recipient ` + testRecipient + ` is not allowed"}`,
			wantWarn:   true,
			wantReason: "upstream_error",
		},
		{
			name:       "body echoing a template id",
			status:     http.StatusConflict,
			body:       `{"error":"` + welcomeTemplateID.String() + `"}`,
			wantWarn:   true,
			wantReason: "upstream_error",
		},
		{
			name:       "empty body",
			status:     http.StatusUnauthorized,
			wantWarn:   true,
			wantReason: "upstream_error",
		},
	}

	for _, send := range mailSendsByID() {
		for _, tc := range cases {
			t.Run(send.kind+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				requests := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests++
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
				}))
				t.Cleanup(srv.Close)
				logger, out := captureWarnings()

				send.call(t.Context(), send.build(srv, logger))

				if requests != 1 {
					t.Fatalf("requests = %d", requests)
				}
				got := out.String()
				assertNoMailMaterial(t, got)
				if !tc.wantWarn {
					if got != "" {
						t.Fatalf("accepted send warned: %s", got)
					}
					return
				}
				if lines := strings.Count(strings.TrimSpace(got), "\n"); lines != 0 {
					t.Fatalf("expected a single line, got: %s", got)
				}
				assertContainsAll(t, got, []string{
					`"event":"skymail_call_failed"`,
					`"level":"warn"`,
					`"kind":"` + send.kind + `"`,
					fmt.Sprintf(`"status":%d`, tc.status),
					`"reason":"` + tc.wantReason + `"`,
				})
				if tc.wantCode == "" {
					if strings.Contains(got, `"code"`) {
						t.Fatalf("log carried a code it could not trust: %s", got)
					}
					return
				}
				if !strings.Contains(got, `"code":"`+tc.wantCode+`"`) {
					t.Fatalf("log missing code %q: %s", tc.wantCode, got)
				}
			})
		}
	}
}

func TestSkyMailSendAddressesTemplateByKeyBeforeID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		key          string
		id           uuid.UUID
		wantRequests int
		wantKey      string
		wantID       string
	}{
		{name: "key only", key: "core.test", wantRequests: 1, wantKey: "core.test"},
		{name: "id only", id: welcomeTemplateID, wantRequests: 1, wantID: welcomeTemplateID.String()},
		{name: "key wins over id", key: "core.test", id: welcomeTemplateID, wantRequests: 1, wantKey: "core.test"},
		{name: "neither addresses anything", wantRequests: 0},
	}
	for _, kind := range []string{"welcome", "certificate"} {
		for _, tc := range cases {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				requests := 0
				var payload map[string]any
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests++
					_ = json.NewDecoder(r.Body).Decode(&payload)
					w.WriteHeader(http.StatusCreated)
				}))
				t.Cleanup(srv.Close)
				logger, out := captureWarnings()
				mailer := &SkyMail{BaseURL: srv.URL, Tokens: StaticToken(testAccessToken), HTTP: srv.Client(), Logger: logger}
				if kind == "welcome" {
					mailer.TemplateKey, mailer.TemplateID = tc.key, tc.id
					mailer.Welcome(t.Context(), testUser())
				} else {
					mailer.CertificateTemplateKey, mailer.CertificateTemplateID = tc.key, tc.id
					mailer.Certificate(t.Context(), testRecipient, "Ada Lovelace", nil)
				}

				if requests != tc.wantRequests {
					t.Fatalf("requests = %d", requests)
				}
				if out.String() != "" {
					t.Fatalf("accepted send warned: %s", out.String())
				}
				if tc.wantRequests == 0 {
					return
				}
				if tc.wantKey == "" {
					if _, ok := payload["template_key"]; ok {
						t.Fatalf("id addressing carried a key: %v", payload)
					}
				} else if payload["template_key"] != tc.wantKey {
					t.Fatalf("template_key = %v", payload["template_key"])
				}
				if tc.wantID == "" {
					if _, ok := payload["template_id"]; ok {
						t.Fatalf("key addressing carried an id, which SkyMail would prefer: %v", payload)
					}
				} else if payload["template_id"] != tc.wantID {
					t.Fatalf("template_id = %v", payload["template_id"])
				}
			})
		}
	}
}

func TestSkyMailSendFallsBackToTemplateIDOnceWhenKeyIsMissing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		key          string
		id           uuid.UUID
		status       int
		wantRequests int
		wantBodies   []string
		wantReason   string
	}{
		{
			name:         "unseeded key falls back to the id",
			key:          "core.test",
			id:           welcomeTemplateID,
			status:       http.StatusNotFound,
			wantRequests: 2,
			wantBodies:   []string{"template_key", "template_id"},
			wantReason:   "template_key_missing_fallback_to_id",
		},
		{
			name:         "no id to fall back to",
			key:          "core.test",
			status:       http.StatusNotFound,
			wantRequests: 1,
			wantBodies:   []string{"template_key"},
			wantReason:   "template_missing",
		},
		{
			name:         "other refusals are not retried",
			key:          "core.test",
			id:           welcomeTemplateID,
			status:       http.StatusInternalServerError,
			wantRequests: 1,
			wantBodies:   []string{"template_key"},
			wantReason:   "upstream_error",
		},
		{
			name:         "a refused id is never retried with a key",
			id:           welcomeTemplateID,
			status:       http.StatusNotFound,
			wantRequests: 1,
			wantBodies:   []string{"template_id"},
			wantReason:   "template_missing",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var bodies []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				bodies = append(bodies, string(raw))
				// The first attempt is refused; anything that follows it is the
				// fallback, and SkyMail accepts that one.
				if len(bodies) > 1 {
					w.WriteHeader(http.StatusCreated)
					return
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"error":"template_not_found"}`)
			}))
			t.Cleanup(srv.Close)
			logger, out := captureWarnings()

			(&SkyMail{
				BaseURL:     srv.URL,
				TemplateKey: tc.key,
				TemplateID:  tc.id,
				Tokens:      StaticToken(testAccessToken),
				HTTP:        srv.Client(),
				Logger:      logger,
			}).Welcome(t.Context(), testUser())

			if len(bodies) != tc.wantRequests {
				t.Fatalf("requests = %d: %v", len(bodies), bodies)
			}
			for i, want := range tc.wantBodies {
				if !strings.Contains(bodies[i], want) {
					t.Fatalf("request %d missing %s: %s", i, want, bodies[i])
				}
			}
			got := out.String()
			assertNoMailMaterial(t, got)
			if !strings.Contains(got, `"reason":"`+tc.wantReason+`"`) {
				t.Fatalf("log missing reason %q: %s", tc.wantReason, got)
			}
			if tc.wantReason != "template_key_missing_fallback_to_id" && strings.Contains(got, "fallback") {
				t.Fatalf("log claimed a fallback that did not happen: %s", got)
			}
		})
	}
}

func TestClientCredentialsWarnsOnRefusedTokenStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		status     int
		body       string
		wantToken  string
		wantWarn   bool
		wantReason string
		wantCode   string
	}{
		{
			name:      "issued token stays quiet",
			status:    http.StatusOK,
			body:      `{"access_token":"` + testAccessToken + `"}`,
			wantToken: testAccessToken,
		},
		{
			name:       "rejected client",
			status:     http.StatusUnauthorized,
			body:       `{"error":"invalid_client"}`,
			wantWarn:   true,
			wantReason: "upstream_error",
			wantCode:   "invalid_client",
		},
		{
			name:       "identity provider failure",
			status:     http.StatusInternalServerError,
			body:       "<html>proxy error</html>",
			wantWarn:   true,
			wantReason: "upstream_error",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(srv.Close)
			logger, out := captureWarnings()

			token, err := ClientCredentials{
				TokenURL:     srv.URL,
				ClientID:     "core",
				ClientSecret: testSecret,
				HTTP:         srv.Client(),
				Logger:       logger,
			}.Token(t.Context())

			got := out.String()
			assertNoMailMaterial(t, got)
			if !tc.wantWarn {
				if err != nil || token != tc.wantToken {
					t.Fatalf("token = %q err = %v", token, err)
				}
				if got != "" {
					t.Fatalf("issued token warned: %s", got)
				}
				return
			}
			if err == nil || token != "" {
				t.Fatalf("refused token = %q err = %v", token, err)
			}
			assertContainsAll(t, got, []string{
				`"event":"skymail_call_failed"`,
				`"level":"warn"`,
				`"kind":"token"`,
				fmt.Sprintf(`"status":%d`, tc.status),
				`"reason":"` + tc.wantReason + `"`,
			})
			if tc.wantCode == "" {
				if strings.Contains(got, `"code"`) {
					t.Fatalf("log carried a code it could not trust: %s", got)
				}
				return
			}
			if !strings.Contains(got, `"code":"`+tc.wantCode+`"`) {
				t.Fatalf("log missing code %q: %s", tc.wantCode, got)
			}
		})
	}
}

func TestSkyMailWarnsOnceForUnconfiguredTemplates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		mailer     func(*log.Logger) *SkyMail
		configured bool
		wantKinds  []string
	}{
		{
			name:      "nothing addressable",
			mailer:    func(logger *log.Logger) *SkyMail { return &SkyMail{Logger: logger} },
			wantKinds: []string{"welcome", "certificate"},
		},
		{
			name: "welcome key only",
			mailer: func(logger *log.Logger) *SkyMail {
				return &SkyMail{TemplateKey: DefaultWelcomeTemplateKey, Logger: logger}
			},
			configured: true,
			wantKinds:  []string{"certificate"},
		},
		{
			name: "certificate id only",
			mailer: func(logger *log.Logger) *SkyMail {
				return &SkyMail{CertificateTemplateID: certificateTemplateID, Logger: logger}
			},
			configured: true,
			wantKinds:  []string{"welcome"},
		},
		{
			name: "both addressable",
			mailer: func(logger *log.Logger) *SkyMail {
				return &SkyMail{
					TemplateKey:            DefaultWelcomeTemplateKey,
					CertificateTemplateKey: DefaultCertificateTemplateKey,
					Logger:                 logger,
				}
			},
			configured: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			logger, out := captureWarnings()
			mailer := tc.mailer(logger)

			mailer.WarnUnconfiguredTemplates()

			if mailer.Configured() != tc.configured {
				t.Fatalf("configured = %v", mailer.Configured())
			}
			got := strings.TrimSpace(out.String())
			if len(tc.wantKinds) == 0 {
				if got != "" {
					t.Fatalf("addressable mailer warned: %s", got)
				}
				return
			}
			lines := strings.Split(got, "\n")
			if len(lines) != len(tc.wantKinds) {
				t.Fatalf("lines = %d: %s", len(lines), got)
			}
			for i, kind := range tc.wantKinds {
				assertContainsAll(t, lines[i], []string{
					`"event":"skymail_template_unconfigured"`,
					`"level":"warn"`,
					`"kind":"` + kind + `"`,
					`"reason":"template_unconfigured"`,
				})
				if strings.Contains(lines[i], `"status"`) {
					t.Fatalf("startup warning carried a status: %s", lines[i])
				}
			}
		})
	}
}

func TestSkyMailWelcomeStaysSilentOnTheSendPathWhenUnconfigured(t *testing.T) {
	t.Parallel()
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	t.Cleanup(srv.Close)
	logger, out := captureWarnings()

	mailer := &SkyMail{BaseURL: srv.URL, Tokens: StaticToken(testAccessToken), HTTP: srv.Client(), Logger: logger}
	mailer.Welcome(t.Context(), testUser())
	mailer.Certificate(t.Context(), testRecipient, "Ada Lovelace", nil)

	if called {
		t.Fatal("unconfigured mailer posted")
	}
	if out.String() != "" {
		t.Fatalf("send path repeated the startup warning: %s", out.String())
	}
}

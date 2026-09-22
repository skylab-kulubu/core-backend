package mail

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestSkyMailCreateListPostsName(t *testing.T) {
	t.Parallel()
	listID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": listID.String(), "name": "GECEKODU SkyDays"})
	}))
	t.Cleanup(srv.Close)

	id, err := (&SkyMail{BaseURL: srv.URL, Tokens: StaticToken("tok"), HTTP: srv.Client()}).CreateList(t.Context(), "GECEKODU SkyDays")
	if err != nil {
		t.Fatal(err)
	}
	if id != listID {
		t.Fatalf("id %v", id)
	}
	if gotAuth != "Bearer tok" || gotPath != "/v1/mailing_lists" {
		t.Fatalf("auth %q path %q", gotAuth, gotPath)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["name"] != "GECEKODU SkyDays" {
		t.Fatalf("body %v", payload)
	}
}

func TestSkyMailGetListNotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	logger, out := captureWarnings()
	err := (&SkyMail{BaseURL: srv.URL, Tokens: StaticToken("tok"), HTTP: srv.Client(), Logger: logger}).GetList(t.Context(), uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd"))
	if out.String() != "" {
		t.Fatalf("a list SkyMail no longer holds is a typed answer, not a warning: %s", out.String())
	}
	if err != ErrListNotFound {
		t.Fatalf("err %v", err)
	}
}

func TestSkyMailDeleteList(t *testing.T) {
	t.Parallel()
	listID := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	if err := (&SkyMail{BaseURL: srv.URL, Tokens: StaticToken("tok"), HTTP: srv.Client()}).DeleteList(t.Context(), listID); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/v1/mailing_lists/"+listID.String() {
		t.Fatalf("method %q path %q", gotMethod, gotPath)
	}
}

func TestSkyMailAddRecipientPostsEmail(t *testing.T) {
	t.Parallel()
	listID := uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	var gotPath string
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"11111111-1111-1111-1111-111111111111","full_name":"Ada","email":"ada@example.com"}`))
	}))
	t.Cleanup(srv.Close)
	if err := (&SkyMail{BaseURL: srv.URL, Tokens: StaticToken("tok"), HTTP: srv.Client()}).AddRecipient(t.Context(), listID, ListRecipient{FullName: "Ada Lovelace", Email: "ada@example.com"}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/mailing_lists/"+listID.String()+"/recipients" {
		t.Fatalf("path %q", gotPath)
	}
	if payload["email"] != "ada@example.com" || payload["full_name"] != "Ada Lovelace" {
		t.Fatalf("payload %v", payload)
	}
}

func TestSkyMailCreateListForbidden(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/mailing_lists" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	logger, out := captureWarnings()
	_, err := (&SkyMail{BaseURL: srv.URL, Tokens: StaticToken("tok"), HTTP: srv.Client(), Logger: logger}).CreateList(t.Context(), "WEBLAB SkyDays")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err %v", err)
	}
	assertContainsAll(t, out.String(), []string{
		`"event":"skymail_call_failed"`,
		`"level":"warn"`,
		`"kind":"list_create"`,
		`"status":403`,
		`"reason":"upstream_error"`,
	})
}

func TestSkyMailListWarnsWithoutNamingTheList(t *testing.T) {
	t.Parallel()
	listID := uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	cases := []struct {
		name     string
		kind     string
		status   int
		body     string
		call     func(*SkyMail) error
		wantWarn bool
	}{
		{
			name:   "recipient add refused",
			kind:   "list_recipient_add",
			status: http.StatusUnprocessableEntity,
			body:   `{"error":"recipient_invalid"}`,
			call: func(m *SkyMail) error {
				return m.AddRecipient(t.Context(), listID, ListRecipient{FullName: "Ada Lovelace", Email: testRecipient})
			},
			wantWarn: true,
		},
		{
			name:     "list read refused",
			kind:     "list_read",
			status:   http.StatusBadGateway,
			call:     func(m *SkyMail) error { return m.GetList(t.Context(), listID) },
			wantWarn: true,
		},
		{
			name:   "missing list stays a typed answer",
			kind:   "list_delete",
			status: http.StatusNotFound,
			call:   func(m *SkyMail) error { return m.DeleteList(t.Context(), listID) },
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

			if err := tc.call(&SkyMail{BaseURL: srv.URL, Tokens: StaticToken(testAccessToken), HTTP: srv.Client(), Logger: logger}); err == nil {
				t.Fatal("refused list call returned no error")
			}
			got := out.String()
			assertNoMailMaterial(t, got)
			if strings.Contains(got, listID.String()) {
				t.Fatalf("log named the list: %s", got)
			}
			if !tc.wantWarn {
				if got != "" {
					t.Fatalf("typed answer warned: %s", got)
				}
				return
			}
			assertContainsAll(t, got, []string{
				`"event":"skymail_call_failed"`,
				`"kind":"` + tc.kind + `"`,
				fmt.Sprintf(`"status":%d`, tc.status),
			})
		})
	}
}

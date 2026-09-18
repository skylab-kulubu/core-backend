package mail

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
	err := (&SkyMail{BaseURL: srv.URL, Tokens: StaticToken("tok"), HTTP: srv.Client()}).GetList(t.Context(), uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd"))
	if err != ErrListNotFound {
		t.Fatalf("err %v", err)
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
	_, err := (&SkyMail{BaseURL: srv.URL, Tokens: StaticToken("tok"), HTTP: srv.Client()}).CreateList(t.Context(), "WEBLAB SkyDays")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err %v", err)
	}
}

package formsfeed

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type fakeTokens struct{ invalidated int }

func (f *fakeTokens) Token(context.Context) (string, error) { return "tok", nil }
func (f *fakeTokens) Invalidate()                           { f.invalidated++ }

func TestClientReadsOneAnswerWithCoresToken(t *testing.T) {
	var path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"formId":"` + applicationForm.String() + `","responseId":"` + responseID.String() + `","status":"accepted","userId":null,
			"hasGuest":true,"submittedAt":"2026-11-20T09:15:00Z","hiddenFields":{"davet":"c"},"version":"2026-11-20T09:16:00.5Z",
			"guest":{"firstName":"Ada","lastName":"Lovelace","email":"ada@example.com"}}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Tokens: &fakeTokens{}}
	got, err := c.Response(context.Background(), applicationForm, responseID)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/internal/v1/forms/"+applicationForm.String()+"/responses/"+responseID.String() || auth != "Bearer tok" {
		t.Fatalf("path %s auth %s", path, auth)
	}
	if got.Guest == nil || got.Guest.Email != "ada@example.com" || got.Version == 0 || got.Invite() != "c" {
		t.Fatalf("got %+v", got)
	}
}

func TestClientErrors(t *testing.T) {
	cases := []struct {
		code       int
		body       string
		want       error
		invalidate int
	}{
		{code: 404, want: ErrNotFound},
		{code: 403, want: ErrForbidden, invalidate: 1},
		{code: 401, invalidate: 1},
		{code: 503},
		{code: 200, body: `{"formId":"` + uuid.NewString() + `","responseId":"` + responseID.String() + `","status":"accepted","version":1}`, want: ErrInvalid},
		{code: 200, body: `[]`, want: ErrInvalid},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.code)
			w.Write([]byte(tc.body))
		}))
		tokens := &fakeTokens{}
		_, err := (&Client{BaseURL: srv.URL, Tokens: tokens}).Response(context.Background(), applicationForm, responseID)
		srv.Close()
		if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) || tokens.invalidated != tc.invalidate {
			t.Fatalf("%d: err %v invalidated %d", tc.code, err, tokens.invalidated)
		}
		if tc.want == nil && (errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden) || errors.Is(err, ErrInvalid)) {
			t.Fatalf("%d: %v should be transient", tc.code, err)
		}
		if strings.Contains(err.Error(), srv.URL) {
			t.Fatalf("the error quotes the address: %v", err)
		}
	}
}

func TestURLFromEnv(t *testing.T) {
	env := map[string]string{urlFallbackEnv: "http://forms:8080/"}
	base, from, err := URLFromEnv(func(k string) string { return env[k] })
	if err != nil || base != "http://forms:8080" || from != urlFallbackEnv {
		t.Fatalf("%q %q %v", base, from, err)
	}
	env[URLEnv] = "http://user:pw@forms"
	if _, from, err := URLFromEnv(func(k string) string { return env[k] }); err == nil || from != URLEnv || strings.Contains(err.Error(), "pw") {
		t.Fatalf("%q %v", from, err)
	}
}

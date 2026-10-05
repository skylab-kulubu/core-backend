package retention

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// A scrubbed click keeps where it came from, the referer's origin, and
// nothing that could name the person: no user information, path, query or
// fragment. The reduction is its own fixed point, so url_hits_scrub never
// finds a scrubbed row due again.
func TestRefererOriginKeepsSchemeAndHostOnly(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	for raw, want := range map[string]string{
		"https://User:Pw@Example.COM:8443/path/ada?token=x#frag": "https://example.com",
		"https://www.instagram.com/":                             "https://www.instagram.com",
		"http://l.facebook.com/l.php?u=https%3A%2F%2Fskyl.app":   "http://l.facebook.com",
		"android-app://com.google.android.gm/":                   "android-app://com.google.android.gm",
		"https://[2001:db8::1]:8080/x":                           "https://[2001:db8::1]",
		"HTTPS://Mixed.Case.Example":                             "https://mixed.case.example",
		"https://a@b@example.com/":                               "https://example.com",
		"https://":                                               "https://",
		"https://example.com":                                    "https://example.com",
		"ada@example.com":                                        "",
		"//example.com/path":                                     "",
		"not a url":                                              "",
		"":                                                       "",
	} {
		var once, twice string
		if err := pool.QueryRow(ctx, `SELECT `+refererOriginSQL("r")+`, `+refererOriginSQL(refererOriginSQL("r"))+` FROM (SELECT $1::text AS r) v`, raw).
			Scan(&once, &twice); err != nil {
			t.Fatal(err)
		}
		if once != want || twice != once {
			t.Errorf("%q: %q then %q, want %q", raw, once, twice, want)
		}
	}
}

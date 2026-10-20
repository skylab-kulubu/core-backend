package feedback_test

import (
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/feedback"
)

func TestCodeIs128BitsBase64URL(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for range 100 {
		code, err := feedback.NewCode()
		if err != nil || len(code) != 22 || !feedback.ValidCode(code) || seen[code] {
			t.Fatalf("code %q %v", code, err)
		}
		seen[code] = true
	}
	for _, bad := range []string{"", "short", strings.Repeat("a", 23), strings.Repeat("a", 21) + "=", strings.Repeat("a", 21) + "/"} {
		if feedback.ValidCode(bad) {
			t.Errorf("%q taken", bad)
		}
	}
}

// The link is the form's own address with the code alone as its query.
func TestLinkCarriesTheCodeAlone(t *testing.T) {
	t.Parallel()
	code := "AAAAAAAAAAAAAAAAAAAAAA"
	for in, want := range map[string]string{
		"https://forms.yildizskylab.com/0b6f1f4e-7f0c-4f43-9d55-6d0a3c1f3a11":                      "https://forms.yildizskylab.com/0b6f1f4e-7f0c-4f43-9d55-6d0a3c1f3a11?davet=" + code,
		" https://forms.yildizskylab.com/f/0b6f1f4e-7f0c-4f43-9d55-6d0a3c1f3a11?utm_source=x#top ": "https://forms.yildizskylab.com/f/0b6f1f4e-7f0c-4f43-9d55-6d0a3c1f3a11?davet=" + code,
	} {
		if got, err := feedback.Link(in, code); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	if _, err := feedback.Link("forms/0b6f1f4e", code); err == nil {
		t.Fatal("relative address taken")
	}
}

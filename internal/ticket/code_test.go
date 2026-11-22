package ticket_test

import (
	"regexp"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

var (
	accessKeyShape = regexp.MustCompile(`^[A-Z2-7]{26}$`)
	codeShape      = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{8}$`)
)

func TestNewAccessKeyAndCodeHaveTheirShapes(t *testing.T) {
	t.Parallel()
	keys, codes := map[string]bool{}, map[string]bool{}
	for range 500 {
		key, err := ticket.NewAccessKey()
		if err != nil || !accessKeyShape.MatchString(key) || !ticket.ValidAccessKey(key) {
			t.Fatalf("access key %q %v", key, err)
		}
		code, err := ticket.NewCode()
		if err != nil || !codeShape.MatchString(code) {
			t.Fatalf("code %q %v", code, err)
		}
		keys[key], codes[code] = true, true
	}
	if len(keys) != 500 || len(codes) < 499 {
		t.Fatalf("%d keys, %d codes of 500", len(keys), len(codes))
	}
}

func TestValidAccessKey(t *testing.T) {
	t.Parallel()
	for key, want := range map[string]bool{
		"ABCDEFGHIJKLMNOPQRSTUVWXYZ":  true,
		"ABCDEFGHIJKLMNOPQRSTUVWX23":  true,
		"abcdefghijklmnopqrstuvwxyz":  false,
		"ABCDEFGHIJKLMNOPQRSTUVWXY1":  false,
		"ABCDEFGHIJKLMNOPQRSTUVWXY":   false,
		"ABCDEFGHIJKLMNOPQRSTUVWXYZA": false,
		"":                            false,
		"../../ABCDEFGHIJKLMNOPQRST":  false,
	} {
		if got := ticket.ValidAccessKey(key); got != want {
			t.Errorf("ValidAccessKey(%q) = %v", key, got)
		}
	}
}

// A code reads the same typed with spaces, a hyphen or none, in small
// letters, and with Crockford's look-alikes (O for 0, I and L for 1).
func TestNormalizeCode(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"K7M2-QX9F":   "K7M2QX9F",
		"k7m2 qx9f":   "K7M2QX9F",
		"K7M2QX9F":    "K7M2QX9F",
		" k7m2-qx9f ": "K7M2QX9F",
		"1O0I-L234":   "1001-1234",
		"io0l-abcd":   "1001-ABCD",
	} {
		got, ok := ticket.NormalizeCode(raw)
		if !ok || got != stripHyphen(want) {
			t.Errorf("NormalizeCode(%q) = %q %v, want %q", raw, got, ok, stripHyphen(want))
		}
	}
	for _, raw := range []string{"", "K7M2-QX9", "K7M2-QX9FF", "K7M2-QX9U", "K7M2-QX9*", "K7M2_QX9F", "ada@example.com", "K7M2-QX9Ş"} {
		if got, ok := ticket.NormalizeCode(raw); ok {
			t.Errorf("NormalizeCode(%q) = %q, want not a code", raw, got)
		}
	}
	if got := ticket.FormatCode("K7M2QX9F"); got != "K7M2-QX9F" {
		t.Errorf("FormatCode = %q", got)
	}
}

func stripHyphen(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '-' {
			out = append(out, s[i])
		}
	}
	return string(out)
}

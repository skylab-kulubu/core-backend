package ticket

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
)

// A Ticket's access key and code (docs/ticket-access-key-and-code.md, ADR
// 0082).
//
// The access key opens the ticket page without a sign-in: who holds the link
// holds the Ticket. It is 16 random bytes in base32 (RFC 4648, no padding),
// 26 capital letters and digits 2–7. Only the holder sees it (their mail,
// `GET /v1/tickets/me`); no panel or list answer carries it.
//
// The code is the short one read out at the desk: 8 characters of
// Crockford's base32 (no I, L, O or U), 40 random bits, shown as XXXX-XXXX.
// It is not a credential: on its own it opens no door and writes no
// check-in. Both are unique across every Ticket.
const (
	// AccessKeyLength is an access key's length in characters.
	AccessKeyLength = 26
	// CodeLength is a code's length without its hyphen.
	CodeLength = 8

	accessKeyBytes = 16
	// crockford is Crockford's base32 alphabet: digits, then the letters
	// without I, L, O and U.
	crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

var accessKeyEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewAccessKey is a fresh access key from crypto/rand.
func NewAccessKey() (string, error) {
	raw := make([]byte, accessKeyBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return accessKeyEncoding.EncodeToString(raw), nil
}

// NewCode is a fresh code from crypto/rand, without its hyphen.
func NewCode() (string, error) {
	raw := make([]byte, CodeLength)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	out := make([]byte, CodeLength)
	for i, b := range raw {
		// 256 is a multiple of 32: every character is equally likely.
		out[i] = crockford[b%32]
	}
	return string(out), nil
}

// ValidAccessKey reports whether key has an access key's shape. A lookup asks
// it first, so a malformed key never reaches the database.
func ValidAccessKey(key string) bool {
	if len(key) != AccessKeyLength {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if !(c >= 'A' && c <= 'Z') && !(c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}

// NormalizeCode reads a code as a person types or says it: spaces and
// hyphens dropped, small letters made capital, and Crockford's look-alikes
// mapped (O to 0, I and L to 1; the Turkish ı and İ are I too). It answers the 8 characters, without the
// hyphen, and whether what was given is a code at all.
func NormalizeCode(raw string) (string, bool) {
	var b strings.Builder
	b.Grow(CodeLength)
	for _, r := range raw {
		switch {
		case r == ' ' || r == '-' || r == '\t':
			continue
		case r >= 'a' && r <= 'z':
			r -= 'a' - 'A'
		}
		switch r {
		case 'O':
			r = '0'
		case 'I', 'L', 'ı', 'İ':
			// Turkish keyboards give ı and İ (caps lock on i): read as I.
			r = '1'
		}
		if r > 0x7f || !strings.ContainsRune(crockford, r) {
			return "", false
		}
		if b.Len() == CodeLength {
			return "", false
		}
		b.WriteRune(r)
	}
	if b.Len() != CodeLength {
		return "", false
	}
	return b.String(), true
}

// FormatCode shows a stored code as XXXX-XXXX. Anything else is answered
// as it is.
func FormatCode(code string) string {
	if len(code) != CodeLength {
		return code
	}
	return code[:4] + "-" + code[4:]
}

// storedCode is the form the database keeps: the code without its hyphen.
func storedCode(code string) string {
	return strings.ReplaceAll(code, "-", "")
}

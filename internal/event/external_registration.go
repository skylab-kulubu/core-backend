package event

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// An Event's external registration link (docs/attendee-import.md): where
// people register for an Event whose sign-up runs on another product's form
// (Microsoft Forms and the like). The site and SkyEvents send "Kayıt ol"
// there; the people who registered come back into core as Tickets through
// the attendee import.

// MaxExternalRegistrationURL is the longest link an Event keeps, in
// characters (the column's check).
const MaxExternalRegistrationURL = 2048

// ErrExternalRegistrationURLInvalid is a link core does not keep: not https,
// no host, a user or password in it, blanks or control characters, or longer
// than MaxExternalRegistrationURL (400 external_registration_url_invalid).
var ErrExternalRegistrationURLInvalid = fmt.Errorf("event: invalid external registration link: %w", ErrInvalid)

// NormalizeExternalRegistrationURL is the link as core keeps it: trimmed,
// "" for none. A link it does not keep is ErrExternalRegistrationURLInvalid.
func NormalizeExternalRegistrationURL(raw string) (string, error) {
	link := strings.TrimSpace(raw)
	if link == "" {
		return "", nil
	}
	// The scheme in lower case, as the column's check reads it.
	if len(link) >= len("https://") && strings.EqualFold(link[:len("https://")], "https://") {
		link = "https://" + link[len("https://"):]
	}
	if !ValidExternalRegistrationURL(link) {
		return "", ErrExternalRegistrationURLInvalid
	}
	return link, nil
}

// ValidExternalRegistrationURL reports whether link may be an Event's
// external registration link: an absolute https address (the scheme in lower
// case) with a host, no
// user information, no blank or control character, at most
// MaxExternalRegistrationURL characters.
func ValidExternalRegistrationURL(link string) bool {
	if link == "" || !utf8.ValidString(link) || utf8.RuneCountInString(link) > MaxExternalRegistrationURL {
		return false
	}
	for _, r := range link {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" {
		return false
	}
	return u.Hostname() != "" && strings.HasPrefix(link, "https://") && !strings.HasPrefix(link, "https:///")
}

// normalizeExternalRegistration is in with its link as core keeps it.
func normalizeExternalRegistration(in Event) (Event, error) {
	link, err := NormalizeExternalRegistrationURL(in.ExternalRegistrationURL)
	if err != nil {
		return Event{}, err
	}
	in.ExternalRegistrationURL = link
	return in, nil
}

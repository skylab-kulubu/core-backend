package event

import (
	"fmt"
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/ticket/theme"
)

// An Event's theme (docs/event-theme.md): the look its PDF ticket, its
// participants' mails and its ticket page take, a key of core's catalogue
// (internal/ticket/theme), none for Kurumsal; and its ticket colour, the
// ticket page's accent.

var (
	// ErrThemeUnknown is a theme that is not a key of the catalogue (400
	// event_theme_unknown).
	ErrThemeUnknown = fmt.Errorf("event: unknown theme: %w", ErrInvalid)
	// ErrTicketAccentColorInvalid is a ticket colour that is not #RRGGBB
	// (400 event_ticket_accent_color_invalid).
	ErrTicketAccentColorInvalid = fmt.Errorf("event: invalid ticket colour: %w", ErrInvalid)
)

// NormalizeTheme is the theme as core keeps it: nil for Kurumsal (none, "",
// or "kurumsal"), else a key of the catalogue; any other is ErrThemeUnknown.
func NormalizeTheme(raw *string) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	key := strings.TrimSpace(*raw)
	switch {
	case key == "" || key == theme.Default:
		return nil, nil
	case !theme.ValidKey(key) || !theme.Known(key):
		return nil, ErrThemeUnknown
	}
	return &key, nil
}

// NormalizeTicketAccentColor is the ticket colour as core keeps it: nil for
// none (none or ""), else #RRGGBB in capitals; any other is
// ErrTicketAccentColorInvalid.
func NormalizeTicketAccentColor(raw *string) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	c := strings.TrimSpace(*raw)
	if c == "" {
		return nil, nil
	}
	if len(c) != 7 || c[0] != '#' {
		return nil, ErrTicketAccentColorInvalid
	}
	for _, r := range c[1:] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return nil, ErrTicketAccentColorInvalid
		}
	}
	c = strings.ToUpper(c)
	return &c, nil
}

// normalizeTheme is in with its theme and ticket colour as core keeps them.
func normalizeTheme(in Event) (Event, error) {
	var err error
	if in.Theme, err = NormalizeTheme(in.Theme); err != nil {
		return Event{}, err
	}
	if in.TicketAccentColor, err = NormalizeTicketAccentColor(in.TicketAccentColor); err != nil {
		return Event{}, err
	}
	return in, nil
}

// readTheme is a stored theme as the Event answers it: nil for Kurumsal,
// and for a key the catalogue no longer has.
func readTheme(stored *string) *string {
	if stored == nil || *stored == theme.Default || !theme.Known(*stored) {
		return nil
	}
	key := *stored
	return &key
}

// ThemeKey is the key of the Event's theme: theme.Default for Kurumsal.
func (e Event) ThemeKey() string {
	if t := readTheme(e.Theme); t != nil {
		return *t
	}
	return theme.Default
}

// LookTheme is the Event's theme of the catalogue.
func (e Event) LookTheme() theme.Theme {
	return theme.Lookup(e.ThemeKey())
}

// bannerTypes are the formats a mail's banner may be: what every mail
// client shows (Outlook shows no WebP).
var bannerTypes = map[string]bool{"image/jpeg": true, "image/png": true}

// BannerURL is the public https address of the Event's poster, its ticket
// image's own picture (docs/ticket-image.md, source upload): its page size
// (media.SizePage, 1200 pixels on the longer side, the original's format),
// or the original when it is smaller; a JPEG or a PNG. Empty when the
// Event chose no picture of its own, its picture cannot be served, it is
// of another format, or the address is not https.
func (e Event) BannerURL() string {
	if !bannerTypes[e.bannerImage.Type()] {
		return ""
	}
	address := media.ConfiguredAddresses().LinkedSizes(e.bannerImage)[media.SizePage].URL
	if !strings.HasPrefix(address, "https://") {
		return ""
	}
	return address
}

// SetBannerImage gives an Event read another way (the memory store, a
// test) its poster's Media, as the Postgres store reads it.
func (e *Event) SetBannerImage(image *media.LinkedImage) {
	e.bannerImage = image
}

// MailLook is the two variables every participant's mail of the Event
// carries (docs/event-theme.md#mails), always, empty for none: Theme, the
// theme's key ("" for Kurumsal), and EventBannerUrl (BannerURL).
func MailLook(e Event) map[string]string {
	key := ""
	if t := readTheme(e.Theme); t != nil {
		key = *t
	}
	return map[string]string{"Theme": key, "EventBannerUrl": e.BannerURL()}
}

// WithMailLook adds MailLook's variables to vars and answers it.
func WithMailLook(vars map[string]string, e Event) map[string]string {
	for k, v := range MailLook(e) {
		vars[k] = v
	}
	return vars
}

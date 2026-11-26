// Package theme is the ticket's look: the PDF core draws and the ticket page
// the club site shows (ADR 0082; docs/ticket-page.md, docs/event-theme.md).
//
// A theme is a set of colours and typefaces on a fixed layout: the QR keeps
// its place and size in every theme, so the door reads it the same way.
// Kurumsal is the plain document of the PDF ticket v5 (Yusuf, 2026-11-22),
// black text on white, the club's purple for its rules, set in Manrope, the
// club site's typeface. What proves a Ticket is the signature in its QR, not
// the look.
//
// An Event picks its theme by key (events.theme, docs/event-theme.md), the
// same key words as the forms service's brand (lower-case letters, digits
// and hyphens, at most 32); none, or a key no longer in the catalogue, is
// Kurumsal. Every theme's own drawing (its typefaces, its CSS over the
// layout, its page head) is in this package alone: a theme changes here and
// nowhere else.
package theme

import (
	_ "embed"
	"regexp"
	"strings"
	"sync"
)

// Default is the key of the theme an Event without one gets.
const Default = "kurumsal"

// Theme is one look of the ticket.
type Theme struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	// Accent is the theme's colour: the page head's rule and the cover band
	// of an Event without a cover. Secondary is its second colour (the
	// catalogue's preview); Kurumsal has one.
	Accent    string `json:"accent"`
	Secondary string `json:"-"`
	// Ink is the text, Muted the labels, Paper the ground, Rule the lines
	// between a programme's rows.
	Ink   string `json:"-"`
	Muted string `json:"-"`
	Paper string `json:"-"`
	Rule  string `json:"-"`
	// Sans is the CSS font stack of the ticket: Manrope, which the PDF
	// embeds, then the fonts of core's PDF renderer (Gotenberg's Chromium).
	// HeadSans is the page head's: a header is drawn as a document of its
	// own, which sees no embedded font.
	Sans     string `json:"-"`
	HeadSans string `json:"-"`
	// PageAccent is the ticket page's colour the theme gives (after the
	// Event's own ticketAccentColor, before its cover's colour); "" for
	// none: Kurumsal's page takes the cover's colour, as before.
	PageAccent string `json:"-"`
	// Look is what the theme draws over the layout; nil draws the layout's
	// own look (Kurumsal).
	Look *Look `json:"-"`
}

// Look is a theme's own drawing over the ticket's fixed layout.
type Look struct {
	// FontCSS is the @font-face rules of the theme's own typefaces, each
	// file embedded as a data URL (the renderer reaches nothing outside the
	// page).
	FontCSS func() string
	// DocumentCSS comes after the layout's own rules.
	DocumentCSS string
	// Band is the band's style without a picture (CSS declarations); ""
	// is the cover's colour, else Accent. A theme with a Band draws it
	// whenever the Event chose no ticket image, its cover's middle never.
	Band string
	// PosterAspect, when set, prints the Event's own ticket image (its
	// "Ayrı görsel") whole across the top, cut about its centre to this
	// width over height, instead of the band: the poster (.kapak.afis).
	// A cover region stays the band it was cut as.
	PosterAspect float64
	// The page head: its text and muted colours, its rule (a CSS border),
	// CSS after its own, its mark (an SVG) and the name beside it, then
	// a muted name after a dot ("" for none).
	HeadInk   string
	HeadMuted string
	HeadRule  string
	HeadCSS   string
	HeadMark  string
	HeadName  string
	HeadSub   string
}

var kurumsal = Theme{
	Key:       Default,
	Name:      "SKY LAB (Kurumsal)",
	Accent:    "#7C3AED",
	Secondary: "#7C3AED",
	Ink:       "#000000",
	Muted:     "#555555",
	Paper:     "#FFFFFF",
	Rule:      "#BDBDBD",
	Sans:      `'Manrope','Noto Sans','Liberation Sans',Arial,sans-serif`,
	HeadSans:  `'Noto Sans','Liberation Sans','Helvetica Neue',Arial,sans-serif`,
}

// The catalogue, in the order the panel lists it: the default first.
var (
	order   = []string{Default, artlab.Key}
	catalog = map[string]Theme{Default: kurumsal, artlab.Key: artlab}
)

var keyShape = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// ValidKey reports whether key has a theme key's shape (the forms
// service's brand rule).
func ValidKey(key string) bool {
	return keyShape.MatchString(key)
}

// Lookup is the theme of key; an empty or unknown key is Kurumsal.
func Lookup(key string) Theme {
	if t, ok := catalog[strings.TrimSpace(key)]; ok {
		return t
	}
	return kurumsal
}

// Known reports whether key names a theme of the catalogue.
func Known(key string) bool {
	_, ok := catalog[key]
	return ok
}

// Catalog is every theme, the default first.
func Catalog() []Theme {
	out := make([]Theme, 0, len(order))
	for _, key := range order {
		out = append(out, catalog[key])
	}
	return out
}

//go:embed skylab-mark.svg
var markSVG string

var (
	markOnce  sync.Once
	markPaths []string
)

// MarkPaths are the SKY LAB mark's path data, in a 586.43 × 553.91 box
// (MarkWidth × MarkHeight), to fill with any colour.
func MarkPaths() []string {
	markOnce.Do(func() {
		for _, m := range regexp.MustCompile(` d="([^"]+)"`).FindAllStringSubmatch(markSVG, -1) {
			markPaths = append(markPaths, m[1])
		}
	})
	return markPaths
}

const (
	MarkWidth  = 586.43
	MarkHeight = 553.91
)

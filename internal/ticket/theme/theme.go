// Package theme is the ticket's look: the PDF core draws and the ticket page
// the club site shows (ADR 0082; docs/ticket-page.md).
//
// A theme is a set of colours and typefaces on a fixed layout: the QR keeps
// its place and size in every theme, so the door reads it the same way.
// Today there is one, Kurumsal: the plain document of the PDF ticket v5
// (Yusuf, 2026-11-22), black text on white, the club's purple for its rules,
// set in Manrope, the club site's typeface. What proves a Ticket is the
// signature in its QR, not the look.
//
// More themes join the catalogue by key, the same key words as the forms
// service's brand (lower-case letters, digits and hyphens, at most 32); an
// unknown key is Kurumsal (bilet-qr-ve-pdf ticket 16 adds the Event's choice).
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
	// Accent is the club's colour: the page head's rule and the cover band
	// of an Event without a cover.
	Accent string `json:"accent"`
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
}

var kurumsal = Theme{
	Key:      Default,
	Name:     "Kurumsal",
	Accent:   "#7C3AED",
	Ink:      "#000000",
	Muted:    "#555555",
	Paper:    "#FFFFFF",
	Rule:     "#BDBDBD",
	Sans:     `'Manrope','Noto Sans','Liberation Sans',Arial,sans-serif`,
	HeadSans: `'Noto Sans','Liberation Sans','Helvetica Neue',Arial,sans-serif`,
}

var catalog = map[string]Theme{Default: kurumsal}

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

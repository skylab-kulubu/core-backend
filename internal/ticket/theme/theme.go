// Package theme is the ticket's look: the PDF core draws and the ticket page
// the club site shows (ADR 0082; docs/ticket-page.md).
//
// A theme is a set of colours, typefaces and ornaments on a fixed layout:
// the QR keeps its place and size in every theme, so the door reads it the
// same way. Today there is one, Kurumsal, an official document's look:
// a guilloche ground drawn from the Ticket's code, the code as the serial
// number, microtext, a seal of the SKY LAB mark and the organizing team's
// logo, and a stub. They are ornament: what proves a Ticket is the
// signature in its QR.
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
	// Accent draws the guilloche, the serial number and the rules.
	Accent string `json:"accent"`
	// Ink is the text, Muted the labels, Paper the ground, Tint the card's
	// ground under the guilloche, Seal the stamp.
	Ink   string `json:"-"`
	Muted string `json:"-"`
	Paper string `json:"-"`
	Tint  string `json:"-"`
	Seal  string `json:"-"`
	// Serif, Sans and Mono are CSS font stacks: the fonts of core's PDF
	// renderer (Gotenberg's Chromium: Noto, Liberation, DejaVu) first.
	Serif string `json:"-"`
	Sans  string `json:"-"`
	Mono  string `json:"-"`
}

var kurumsal = Theme{
	Key:    Default,
	Name:   "Kurumsal",
	Accent: "#1F4E79",
	Ink:    "#14213D",
	Muted:  "#56627A",
	Paper:  "#FFFFFF",
	Tint:   "#F4F7FA",
	Seal:   "#8E1B2C",
	Serif:  `'Noto Serif','Liberation Serif',Georgia,'Times New Roman',serif`,
	Sans:   `'Noto Sans','Liberation Sans','Helvetica Neue',Helvetica,Arial,sans-serif`,
	Mono:   `'DejaVu Sans Mono','Liberation Mono',Menlo,Consolas,monospace`,
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

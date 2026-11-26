package handlers

import (
	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/ticket/theme"
)

// EventThemePreview is a theme's colours for the panel's list: its ground,
// its text and its two accents.
type EventThemePreview struct {
	Paper     string `json:"paper"`
	Ink       string `json:"ink"`
	Accent    string `json:"accent"`
	Secondary string `json:"secondary"`
}

// EventThemeView is one theme of the catalogue (docs/event-theme.md).
type EventThemeView struct {
	Key     string            `json:"key"`
	Name    string            `json:"name"`
	Default bool              `json:"default"`
	Preview EventThemePreview `json:"preview"`
}

// EventThemes answers GET /v1/event-themes: the themes an Event may pick,
// the default (Kurumsal) first. It is the same for everyone, signed in or
// not, and changes only with a release.
func EventThemes(c fiber.Ctx) error {
	out := make([]EventThemeView, 0, 2)
	for _, t := range theme.Catalog() {
		out = append(out, EventThemeView{
			Key: t.Key, Name: t.Name, Default: t.Key == theme.Default,
			Preview: EventThemePreview{Paper: t.Paper, Ink: t.Ink, Accent: t.Accent, Secondary: t.Secondary},
		})
	}
	c.Set(fiber.HeaderCacheControl, "public, max-age=300")
	return c.JSON(out)
}

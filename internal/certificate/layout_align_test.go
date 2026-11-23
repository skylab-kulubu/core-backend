package certificate

import (
	"context"
	"strings"
	"testing"
)

// A shrink-to-fit box is a flex box, where text-align does not move a
// line shorter than the box: the alignment has to reach justify-content,
// or a centred name prints at the left edge.
func TestFitBoxKeepsItsAlignment(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":       "justify-content:flex-start;",
		"left":   "justify-content:flex-start;",
		"center": "justify-content:safe center;",
		"right":  "justify-content:safe flex-end;",
	}
	for align, want := range cases {
		align, want := align, want
		t.Run("align="+align, func(t *testing.T) {
			t.Parallel()
			layout := Layout{Width: 800, Height: 600, Orientation: "landscape", Elements: []Element{
				{ID: "name", Kind: "recipientName", X: 10, Y: 60, Width: 400, Height: 40, FontSize: 16, Align: align, Fit: "shrink"},
				{ID: "event", Kind: "eventName", X: 10, Y: 110, Width: 400, Height: 40, FontSize: 16, Align: align},
				{ID: "qr", Kind: "verificationQr", X: 500, Y: 10, Width: 100, Height: 100},
			}}
			html, err := LayoutHTML(context.Background(), layout, PreviewData{RecipientName: "Ada", EventName: "Etkinlik"}, "https://skyl.app/c/X", nil)
			if err != nil {
				t.Fatal(err)
			}
			start := strings.Index(html, `<div class="fit"`)
			if start < 0 {
				t.Fatalf("no fit box: %s", html)
			}
			box := html[start : start+strings.Index(html[start:], ">Ada</div>")]
			if !strings.Contains(box, want) {
				t.Fatalf("fit box has no %q: %s", want, box)
			}
			if strings.Count(html, "justify-content:") != 1 {
				t.Fatalf("justify-content outside the one fit box: %s", html)
			}
			if strings.Contains(html, "justify-content:inherit") {
				t.Fatalf(".fit still inherits justify-content: %s", html)
			}
		})
	}
}

// A fitted box is one line that shrinks, as the editor draws it. The
// element's inline pre-wrap would beat the .fit rule's nowrap and break
// a long name over two lines, so the fit box must say nowrap last; a box
// without fit keeps pre-wrap.
func TestFitBoxStaysOnOneLine(t *testing.T) {
	t.Parallel()
	layout := Layout{Width: 800, Height: 600, Orientation: "landscape", Elements: []Element{
		{ID: "name", Kind: "recipientName", X: 10, Y: 60, Width: 200, Height: 40, FontSize: 24, Align: "center", Fit: "shrink", MinFontSize: 8},
		{ID: "event", Kind: "eventName", X: 10, Y: 110, Width: 200, Height: 40, FontSize: 16, Align: "center"},
		{ID: "team", Kind: "ownerTeam", X: 10, Y: 160, Width: 60, Height: 20, FontSize: 12, Fit: "shrink"},
		{ID: "qr", Kind: "verificationQr", X: 500, Y: 10, Width: 100, Height: 100},
	}}
	data := PreviewData{RecipientName: "Muhammed Abdurrahman Yıldızoğlu", EventName: "Etkinlik", OwnerTeam: "WEBLAB"}
	html, err := LayoutHTML(context.Background(), layout, data, "https://skyl.app/c/X", nil)
	if err != nil {
		t.Fatal(err)
	}
	lastWhiteSpace := func(style string) string {
		i := strings.LastIndex(style, "white-space:")
		if i < 0 {
			return ""
		}
		rest := style[i+len("white-space:"):]
		return rest[:strings.Index(rest, ";")]
	}
	styleOf := func(text string) string {
		end := strings.Index(html, ">"+text+"</div>")
		if end < 0 {
			t.Fatalf("no box for %q: %s", text, html)
		}
		start := strings.LastIndex(html[:end], "<div")
		return html[start:end]
	}
	fitBoxes := 0
	for rest := html; ; {
		i := strings.Index(rest, `<div class="fit"`)
		if i < 0 {
			break
		}
		rest = rest[i:]
		box := rest[:strings.Index(rest, ">")]
		rest = rest[len(box):]
		fitBoxes++
		if got := lastWhiteSpace(box); got != "nowrap" {
			t.Fatalf("fit box ends with white-space %q, want nowrap: %s", got, box)
		}
	}
	if fitBoxes != 2 {
		t.Fatalf("checked %d fit boxes, want 2 (name, team): %s", fitBoxes, html)
	}
	if got := lastWhiteSpace(styleOf(data.EventName)); got != "pre-wrap" {
		t.Fatalf("box without fit has white-space %q, want pre-wrap", got)
	}
}

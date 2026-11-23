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
		"center": "justify-content:center;",
		"right":  "justify-content:flex-end;",
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

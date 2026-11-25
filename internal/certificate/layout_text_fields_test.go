package certificate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func textFieldsLayout(el Element) Layout {
	return Layout{Width: 1123, Height: 794, Orientation: "landscape", Elements: []Element{
		el,
		{ID: "name", Kind: "recipientName", X: 100, Y: 300, Width: 900, Height: 80, FontSize: 48},
		{ID: "serial", Kind: "serial", X: 100, Y: 700, Width: 400, Height: 30, FontSize: 12},
	}}
}

// The converted-text fields: origin, fontStyle and sourceFont are checked;
// the original background needs a background.
func TestValidateLayoutTextFields(t *testing.T) {
	t.Parallel()
	base := Element{ID: "t", Kind: "staticText", Text: "Başarı Belgesi", X: 10, Y: 10, Width: 300, Height: 50, FontSize: 30}
	good := base
	good.Origin, good.FontStyle, good.SourceFont = OriginBackgroundText, FontStyleItalic, strings.Repeat("ş", 120)
	if err := ValidateLayout(textFieldsLayout(good)); err != nil {
		t.Fatalf("good: %v", err)
	}
	for name, change := range map[string]func(*Element){
		"origin":     func(el *Element) { el.Origin = "upload" },
		"fontStyle":  func(el *Element) { el.FontStyle = "oblique" },
		"sourceFont": func(el *Element) { el.SourceFont = strings.Repeat("a", 121) },
		"utf8":       func(el *Element) { el.SourceFont = "\xff" },
	} {
		el := base
		change(&el)
		var refused *InvalidError
		if err := ValidateLayout(textFieldsLayout(el)); !errors.As(err, &refused) || refused.Code != CodeLayoutElementStyle {
			t.Errorf("%s: %v", name, err)
		}
	}
	original, background := uuid.New(), uuid.New()
	layout := textFieldsLayout(base)
	layout.BackgroundOriginalMediaID = &original
	if err := ValidateLayout(layout); !errors.Is(err, ErrInvalid) {
		t.Fatalf("original without a background: %v", err)
	}
	layout.BackgroundMediaID = &background
	if err := ValidateLayout(layout); err != nil {
		t.Fatalf("original with a background: %v", err)
	}
	// Linked (kept, checked) but never an asset of a version.
	if ids := layoutAssetIDs(layout); len(ids) != 1 || ids[0] != background {
		t.Fatalf("asset ids %v", ids)
	}
	if ids := layoutLinkedIDs(layout); len(ids) != 2 {
		t.Fatalf("linked ids %v", ids)
	}
}

// A layout written before these fields keeps its checksum and its page;
// an italic element says font-style, no other does.
func TestTextFieldsLeaveOldLayoutsAlone(t *testing.T) {
	t.Parallel()
	el := Element{ID: "t", Kind: "staticText", Text: "Katılım", X: 10, Y: 10, Width: 300, Height: 50, FontSize: 30, FontFamily: "Montserrat"}
	layout := textFieldsLayout(el)
	raw, err := marshalLayout(layout)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"origin", "sourceFont", "fontStyle", "backgroundOriginalMediaId"} {
		if strings.Contains(string(raw), field) {
			t.Fatalf("an old layout writes %s: %s", field, raw)
		}
	}
	old := `{"width":1123,"height":794,"orientation":"landscape","backgroundColor":"","elements":[` +
		`{"id":"t","kind":"staticText","text":"Katılım","x":10,"y":10,"width":300,"height":50,"fontFamily":"Montserrat","fontSize":30},` +
		`{"id":"name","kind":"recipientName","x":100,"y":300,"width":900,"height":80,"fontSize":48},` +
		`{"id":"serial","kind":"serial","x":100,"y":700,"width":400,"height":30,"fontSize":12}]}`
	if string(raw) != old {
		t.Fatalf("layout JSON\n%s\nwant\n%s", raw, old)
	}
	page, err := LayoutHTML(context.Background(), layout, PreviewData{RecipientName: "Ayşe"}, "https://skyl.app/c/X", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, "overflow:hidden;font-style:italic;") {
		t.Fatal("a normal element says font-style")
	}
	if style := elementStyle(el); !strings.HasSuffix(style, "overflow:hidden;") {
		t.Fatalf("style %q", style)
	}
	el.FontStyle = FontStyleItalic
	if style := elementStyle(el); !strings.HasSuffix(style, "overflow:hidden;font-style:italic;") {
		t.Fatalf("italic style %q", style)
	}
}

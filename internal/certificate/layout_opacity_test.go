package certificate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// A layout that leaves an element's opacity out (one sent to the API by
// hand, an older draft) prints the element fully visible, as the panel's
// editor shows it; an explicit 0 stays invisible, also after the layout is
// stored and read back. A layout without opacity is stored byte for byte
// as before, so its checksum does not change.
func TestLayoutOpacityLeftOutIsOne(t *testing.T) {
	t.Parallel()
	raw := `{"width":800,"height":600,"orientation":"landscape","elements":[
		{"id":"name","kind":"recipientName","x":10,"y":10,"width":400,"height":40,"fontSize":20},
		{"id":"event","kind":"eventName","x":10,"y":60,"width":400,"height":40,"fontSize":20,"opacity":0},
		{"id":"label","kind":"staticText","text":"Katılım","x":10,"y":110,"width":400,"height":40,"fontSize":20,"opacity":0.5},
		{"id":"qr","kind":"verificationQr","x":500,"y":10,"width":100,"height":100}]}`
	var layout Layout
	if err := json.Unmarshal([]byte(raw), &layout); err != nil {
		t.Fatal(err)
	}
	stored, err := marshalLayout(layout)
	if err != nil {
		t.Fatal(err)
	}
	var reread Layout
	if err := json.Unmarshal(stored, &reread); err != nil {
		t.Fatal(err)
	}
	html, err := LayoutHTML(context.Background(), reread, PreviewData{RecipientName: "Ada", EventName: "Etkinlik"}, "https://skyl.app/c/X", nil)
	if err != nil {
		t.Fatal(err)
	}
	for text, want := range map[string]string{">Ada</div>": "opacity:1;", ">Etkinlik</div>": "opacity:0;", ">Katılım</div>": "opacity:0.5;"} {
		end := strings.Index(html, text)
		if end < 0 {
			t.Fatalf("no %q in %s", text, html)
		}
		start := strings.LastIndex(html[:end], "<div")
		if !strings.Contains(html[start:end], want) {
			t.Fatalf("%s: want %q in %s", text, want, html[start:end])
		}
	}
	if qr := html[strings.Index(html, "<img"):]; !strings.Contains(qr[:strings.Index(qr, "src=")], "opacity:1;") {
		t.Fatalf("the QR code left without opacity is not visible: %s", qr)
	}

	plain := Layout{Width: 800, Height: 600, Orientation: "landscape", Elements: []Element{{ID: "name", Kind: "recipientName", X: 10, Y: 10, Width: 400, Height: 40}}}
	out, err := marshalLayout(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "opacity") {
		t.Fatalf("a layout without opacity is stored differently: %s", out)
	}
	if err := ValidateLayout(Layout{Width: 800, Height: 600, Orientation: "landscape", Elements: append(append([]Element(nil), reread.Elements...), Element{ID: "x", Kind: "shape", X: 1, Y: 1, Width: 1, Height: 1, Opacity: ptr(1.5)})}); err == nil {
		t.Fatal("an opacity over 1 is accepted")
	}
}

func ptr(v float64) *float64 { return &v }

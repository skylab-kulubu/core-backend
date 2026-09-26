package media_test

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// svgService uploads for the purposes that list SVG, as a core whose CMS
// has a service client, so that cms_image can be uploaded.
func svgService(t *testing.T) (media.Service, *media.MemoryBlob) {
	t.Helper()
	blobs := media.NewMemoryBlob()
	return media.NewServiceWithOptions(media.NewMemoryStore(), blobs, authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test",
		media.ServiceOptions{ServiceProducts: []authz.Product{authz.ProductCMS}}), blobs
}

// organizer may upload Event covers and gallery photos.
func organizer() authz.Principal {
	return authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
}

// svgElements lists an SVG's elements in document order, each with its
// sorted attributes (namespace-qualified), as a reader sees the document.
func svgElements(t *testing.T, data []byte) []string {
	t.Helper()
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var out []string
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("stored SVG does not parse: %v\n%s", err, data)
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		var attrs []string
		for _, a := range start.Attr {
			if a.Name.Space == "xmlns" || a.Name.Local == "xmlns" {
				continue
			}
			attrs = append(attrs, a.Name.Space+":"+a.Name.Local+"="+a.Value)
		}
		slices.Sort(attrs)
		out = append(out, start.Name.Local+" "+strings.Join(attrs, " "))
	}
}

const benignLogo = `<?xml version="1.0" encoding="UTF-8"?>
<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" viewBox="0 0 120 60" width="120" height="60">
  <title>Sky Lab</title>
  <defs>
    <linearGradient id="g" x1="0" y1="0" x2="1" y2="0">
      <stop offset="0" stop-color="#0a3d91"/>
      <stop offset="1" stop-color="#3fa9f5" stop-opacity="0.8"/>
    </linearGradient>
    <clipPath id="c"><rect width="120" height="60" rx="8"/></clipPath>
    <symbol id="star" viewBox="0 0 10 10"><polygon points="5,0 6,4 10,4 7,6 8,10 5,7 2,10 3,6 0,4 4,4"/></symbol>
  </defs>
  <style>.t { font-family: sans-serif; font-weight: 700; }</style>
  <g clip-path="url(#c)" transform="translate(0 0)">
    <rect width="120" height="60" fill="url(#g)"/>
    <path d="M10 50 L30 10 L50 50 Z" fill="#fff" stroke="#000" stroke-width="2"/>
    <use xlink:href="#star" x="80" y="10" width="20" height="20"/>
    <text class="t" x="60" y="40" text-anchor="middle" style="fill: #ffffff; font-size: 14px">SKY <tspan dx="2">LAB</tspan></text>
  </g>
</svg>`

func TestService_SVGIsStoredSanitizedAsASVGDownload(t *testing.T) {
	t.Parallel()
	svc, blobs := svgService(t)
	hostile := `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" viewBox="0 0 10 10" onload="alert(1)">
  <script>alert(document.cookie)</script>
  <script xlink:href="https://evil.example/x.js"/>
  <foreignObject width="10" height="10"><iframe xmlns="http://www.w3.org/1999/xhtml" src="https://evil.example/"/></foreignObject>
  <image href="data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==" width="10" height="10"/>
  <a href="javascript:alert(1)"><rect width="5" height="5"/></a>
  <use href="https://evil.example/sprite.svg#icon"/>
  <use xlink:href="javascript:alert(1)"/>
  <animate attributeName="href" to="javascript:alert(1)"/>
  <set attributeName="onclick" to="alert(1)"/>
  <style>@import url("https://evil.example/x.css"); rect { fill: url(https://evil.example/p.svg#p); width: expression(alert(1)); }</style>
  <rect width="10" height="10" onclick="alert(1)" fill="url(https://evil.example/p.svg#p)" style="fill: url('https://evil.example/i.png'); stroke: red"/>
  <circle cx="5" cy="5" r="2" fill="url(#ok)" data-x="javascript:alert(1)"/>
  <style>@im<!-- split -->port url("https://evil.example/split.css"); circle { fill: u<!-- split -->rl(https://evil.example/q.svg#q) }</style>
</svg>`

	created, err := svc.UploadForPurpose(context.Background(), organizer(), "event_cover", uploaded("cover.svg", "image/svg+xml", []byte(hostile)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(created.Key, ".svg") || created.Type != "image/svg+xml" || created.Kind != media.KindImage {
		t.Fatalf("created %+v", created)
	}
	meta, _ := blobs.Metadata(created.Key)
	if meta.ContentType != "image/svg+xml" || !strings.HasPrefix(meta.ContentDisposition, "attachment") {
		t.Fatalf("served as %+v, want an SVG download", meta)
	}
	stored, _ := blobs.Get(created.Key)
	lower := strings.ToLower(string(stored))
	for _, gone := range []string{"<script", "foreignobject", "<iframe", "<image", "<animate", "<set", "<a ", "onload", "onclick", "javascript:", "evil.example", "@import", "expression", "data:"} {
		if strings.Contains(lower, gone) {
			t.Errorf("the stored SVG still carries %q:\n%s", gone, stored)
		}
	}
	elements := svgElements(t, stored)
	if !slices.ContainsFunc(elements, func(e string) bool { return strings.HasPrefix(e, "rect ") && strings.Contains(e, "stroke: red") }) {
		t.Errorf("the harmless rect went too: %v", elements)
	}
	if keys := blobs.Keys(); len(keys) != 1 {
		t.Fatalf("stored %v, want the SVG alone", keys)
	}
	for _, size := range []string{"card", "page"} {
		if got := created.Sizes[size]; got.URL != created.URL {
			t.Errorf("%s: %+v, want the SVG itself", size, got)
		}
	}
}

// A logo that uses nothing hostile keeps every element and attribute it
// has: re-serialized, not passed through.
func TestService_SVGSanitizerKeepsABenignLogo(t *testing.T) {
	t.Parallel()
	svc, blobs := svgService(t)

	created, err := svc.UploadForPurpose(context.Background(), signedIn("87878787-8787-8787-8787-878787878787"), "cms_image", uploaded("logo.svg", "image/svg+xml", []byte(benignLogo)))
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := blobs.Get(created.Key)
	if bytes.Equal(stored, []byte(benignLogo)) {
		t.Fatal("the SVG was stored as uploaded, not re-serialized")
	}
	want, got := svgElements(t, []byte(benignLogo)), svgElements(t, stored)
	if !slices.Equal(want, got) {
		t.Fatalf("elements changed:\nwant %v\ngot  %v", want, got)
	}
	if !bytes.Contains(stored, []byte("SKY ")) || !bytes.Contains(stored, []byte("font-weight: 700")) {
		t.Fatalf("text or style lost:\n%s", stored)
	}
}

func TestService_SVGThatCoreRefuses(t *testing.T) {
	t.Parallel()
	svc, blobs := svgService(t)
	p := organizer()
	for name, svg := range map[string]string{
		"a document type":   `<!DOCTYPE svg PUBLIC "-//W3C//DTD SVG 1.1//EN" "http://www.w3.org/Graphics/SVG/1.1/DTD/svg11.dtd"><svg xmlns="http://www.w3.org/2000/svg"/>`,
		"an entity bomb":    `<!DOCTYPE svg [<!ENTITY a "aaaaaaaaaa"><!ENTITY b "&a;&a;&a;&a;&a;&a;&a;&a;&a;&a;">]><svg xmlns="http://www.w3.org/2000/svg"><text>&b;</text></svg>`,
		"an unknown entity": `<svg xmlns="http://www.w3.org/2000/svg"><text>&lol;</text></svg>`,
		"not XML":           `<svg xmlns="http://www.w3.org/2000/svg"><rect`,
		"not an SVG root":   `<html><svg xmlns="http://www.w3.org/2000/svg"/></html>`,
		"too many elements": `<svg xmlns="http://www.w3.org/2000/svg">` + strings.Repeat(`<rect/>`, 10001) + `</svg>`,
		"too deep":          `<svg xmlns="http://www.w3.org/2000/svg">` + strings.Repeat(`<g>`, 70) + strings.Repeat(`</g>`, 70) + `</svg>`,
	} {
		_, err := svc.UploadForPurpose(context.Background(), p, "event_gallery", uploaded("x.svg", "image/svg+xml", []byte(svg)))
		if !errors.Is(err, media.ErrTypeNotAllowed) {
			t.Errorf("%s: err = %v, want %v", name, err, media.ErrTypeNotAllowed)
		}
	}
	huge := `<svg xmlns="http://www.w3.org/2000/svg"><!--` + strings.Repeat("x", 1<<20) + `--></svg>`
	_, err := svc.UploadForPurpose(context.Background(), p, "event_gallery", uploaded("x.svg", "image/svg+xml", []byte(huge)))
	var refusal *media.PurposeRefusal
	if !errors.Is(err, media.ErrTooLarge) || !errors.As(err, &refusal) || refusal.MaxBytes != 1<<20 {
		t.Fatalf("an SVG over 1 MiB: err = %v", err)
	}
	if keys := blobs.Keys(); len(keys) != 0 {
		t.Fatalf("refused SVGs left %v", keys)
	}
}

// SVG only where the catalogue lists it: CMS images and Event pictures, not
// profile pictures.
func TestService_SVGOnlyForThePurposesThatListIt(t *testing.T) {
	t.Parallel()
	svc, _ := svgService(t)
	logo := []byte(benignLogo)
	for purpose, p := range map[string]authz.Principal{"cms_image": signedIn("88888888-8888-8888-8888-000000000088"), "event_cover": organizer(), "event_gallery": organizer()} {
		if _, err := svc.UploadForPurpose(context.Background(), p, purpose, uploaded("logo.svg", "image/svg+xml", logo)); err != nil {
			t.Errorf("%s: %v", purpose, err)
		}
	}
	_, err := svc.UploadForPurpose(context.Background(), signedIn("89898989-8989-8989-8989-898989898989"), "profile_picture", uploaded("me.svg", "image/svg+xml", logo))
	var refusal *media.PurposeRefusal
	if !errors.Is(err, media.ErrTypeNotAllowed) || !errors.As(err, &refusal) || slices.Contains(refusal.AllowedTypes, "image/svg+xml") {
		t.Fatalf("profile picture: err = %v", err)
	}
}

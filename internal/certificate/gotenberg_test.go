package certificate

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestGotenbergPostsHTML(t *testing.T) {
	t.Parallel()
	var gotPath, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotType = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		if r.FormValue("printBackground") != "true" || r.FormValue("preferCssPageSize") != "true" {
			t.Errorf("missing deterministic HTML render flags")
		}
		file, _, err := r.FormFile("files")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		body, _ := io.ReadAll(file)
		if !strings.Contains(string(body), "ARTLAB") {
			t.Errorf("missing HTML file")
		}
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.4 g"))
	}))
	t.Cleanup(srv.Close)
	g := &Gotenberg{BaseURL: srv.URL, HTTP: srv.Client()}
	pdf, err := g.PDF(t.Context(), "<html>ARTLAB</html>")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/forms/chromium/convert/html" {
		t.Fatalf("path %q", gotPath)
	}
	if !strings.HasPrefix(gotType, "multipart/form-data") {
		t.Fatalf("type %s", gotType)
	}
	if string(pdf) != "%PDF-1.4 g" {
		t.Fatalf("pdf %q", pdf)
	}
}

func TestGotenbergUsesSinglePagePDFAsVectorBackground(t *testing.T) {
	t.Parallel()
	background := []byte("%PDF-1.7 vector background")
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/forms/pdfengines/metadata/read":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			file, _, err := r.FormFile("files")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			got, _ := io.ReadAll(file)
			if !bytes.Equal(got, background) {
				t.Fatalf("background = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"background.pdf":{"PageCount":1,"MIMEType":"application/pdf"}}`)
		case "/forms/chromium/convert/html":
			if err := r.ParseMultipartForm(2 << 20); err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]string{
				"printBackground":     "true",
				"omitBackground":      "true",
				"preferCssPageSize":   "true",
				"watermarkSource":     "pdf",
				"watermarkExpression": "background.pdf",
			} {
				if got := r.FormValue(key); got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
			file, header, err := r.FormFile("watermark")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if header.Filename != "background.pdf" {
				t.Fatalf("watermark filename = %q", header.Filename)
			}
			got, _ := io.ReadAll(file)
			if !bytes.Equal(got, background) {
				t.Fatalf("watermark = %q", got)
			}
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write([]byte("%PDF-1.7 composed"))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	g := &Gotenberg{BaseURL: srv.URL, HTTP: srv.Client()}
	pdf, err := g.PDFWithBackground(t.Context(), "<html>overlay</html>", background)
	if err != nil {
		t.Fatal(err)
	}
	if string(pdf) != "%PDF-1.7 composed" || requests != 2 {
		t.Fatalf("pdf=%q requests=%d", pdf, requests)
	}
}

func TestGotenbergRejectsMultiPagePDFBackground(t *testing.T) {
	t.Parallel()
	converted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/forms/chromium/convert/html" {
			converted = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"background.pdf":{"PageCount":2,"MIMEType":"application/pdf"}}`)
	}))
	t.Cleanup(srv.Close)

	g := &Gotenberg{BaseURL: srv.URL, HTTP: srv.Client()}
	if _, err := g.PDFWithBackground(t.Context(), "<html>overlay</html>", []byte("%PDF-1.7 two pages")); err == nil {
		t.Fatal("multi-page PDF background must be rejected")
	}
	if converted {
		t.Fatal("invalid background must not be rendered")
	}
}

// The ticket PDF asks for a tagged PDF; the certificate's does not change.
func TestGotenbergTaggedPDF(t *testing.T) {
	t.Parallel()
	tagged := map[string]string{}
	headers := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		files := r.MultipartForm.File["files"]
		if len(files) == 0 || files[0].Filename != "index.html" {
			t.Fatalf("files %v", files)
		}
		read := func(i int) string {
			f, err := files[i].Open()
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			body, _ := io.ReadAll(f)
			return string(body)
		}
		body := read(0)
		tagged[body] = r.FormValue("generateTaggedPdf")
		for i, f := range files[1:] {
			if f.Filename != "header.html" {
				t.Fatalf("file %q", f.Filename)
			}
			headers[body] = read(i + 1)
		}
		_, _ = w.Write([]byte("%PDF-1.7 t"))
	}))
	t.Cleanup(srv.Close)
	g := &Gotenberg{BaseURL: srv.URL, HTTP: srv.Client()}
	if _, err := g.TaggedPDF(t.Context(), "<html>bilet</html>", "<html>Sayfa <span class=\"pageNumber\"></span></html>"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.TaggedPDF(t.Context(), "<html>başlıksız</html>", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := g.PDF(t.Context(), "<html>sertifika</html>"); err != nil {
		t.Fatal(err)
	}
	if tagged["<html>bilet</html>"] != "true" || tagged["<html>sertifika</html>"] != "" {
		t.Fatalf("generateTaggedPdf %v", tagged)
	}
	// The ticket's page head goes as header.html; the others send none.
	if len(headers) != 1 || headers["<html>bilet</html>"] != "<html>Sayfa <span class=\"pageNumber\"></span></html>" {
		t.Fatalf("headers %v", headers)
	}
}

// The map's picture: the page, its size, a JPEG of the quality, taken once
// the fonts are in; anything but a JPEG back is an error.
func TestGotenbergScreenshot(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var got map[string]string
	var path, page string
	answer := []byte{0xff, 0xd8, 0xff, 0xe0, 'j'}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		path = r.URL.Path
		got = map[string]string{}
		for k, v := range r.MultipartForm.Value {
			got[k] = v[0]
		}
		file, _, err := r.FormFile("files")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(file)
		page = string(body)
		_, _ = w.Write(answer)
	}))
	t.Cleanup(srv.Close)
	g := &Gotenberg{BaseURL: srv.URL, HTTP: srv.Client()}
	shot, err := g.Screenshot(t.Context(), "<html>harita</html>", 1280, 480, 82)
	if err != nil || string(shot) != string(answer) {
		t.Fatalf("%q %v", shot, err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := map[string]string{"width": "1280", "height": "480", "clip": "true", "format": "jpeg", "quality": "82", "waitDelay": "500ms"}
	if path != "/forms/chromium/screenshot/html" || page != "<html>harita</html>" || len(got) != len(want) {
		t.Fatalf("%s %q %v", path, page, got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q", k, got[k])
		}
	}
	answer = []byte("<html>bir hata</html>")
	mu.Unlock()
	defer mu.Lock()
	if _, err := g.Screenshot(t.Context(), "<html>harita</html>", 1280, 480, 82); err == nil {
		t.Fatal("a page that is not a JPEG was taken")
	}
	if _, err := g.Screenshot(t.Context(), "<html>harita</html>", 0, 480, 82); err == nil {
		t.Fatal("no width")
	}
}

package certificate

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

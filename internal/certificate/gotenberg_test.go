package certificate

import (
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
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "index.html") {
			t.Errorf("missing file part")
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

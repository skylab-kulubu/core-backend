package certificate_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/certificate"
)

// pdfToolsContract answers like pdf-tools (pdf-tools/README.md): a
// multipart body with the PDF in "file" and JSON in "params", a
// Content-Length (no chunked bodies), RFC 9457 problems with a code.
type pdfToolsContract struct {
	widthPt, heightPt float64
	pages             int
	encrypted         bool
	textCount         int
	status            int    // of every route, when not 0
	code              string // the problem's code, with status
	retryAfter        string
	placeAnswer       []byte // what /v1/place answers, when set
	renderAnswer      []byte // what /v1/render answers, when set

	down                        atomic.Bool // every route answers 503 busy
	inspected, placed, rendered atomic.Int32
	mu                          sync.Mutex
	places                      []map[string]any
	renderParams                []map[string]any
}

func (p *pdfToolsContract) calls() int32 {
	return p.inspected.Load() + p.placed.Load() + p.rendered.Load()
}

// placedPDF is what the contract's /v1/place answers by default.
const placedPDF = "%PDF-1.7 placed on the page"

func (p *pdfToolsContract) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		problem := func(status int, code string) {
			w.Header().Set("Content-Type", "application/problem+json")
			if p.retryAfter != "" {
				w.Header().Set("Retry-After", p.retryAfter)
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "about:blank", "title": http.StatusText(status), "status": status, "code": code})
		}
		if r.Method != http.MethodPost {
			problem(http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		if r.ContentLength <= 0 {
			problem(http.StatusLengthRequired, "length_required")
			return
		}
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			problem(http.StatusBadRequest, "bad_request")
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			problem(http.StatusBadRequest, "bad_request")
			return
		}
		pdf, _ := io.ReadAll(file)
		var params map[string]any
		if raw := r.FormValue("params"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &params); err != nil {
				problem(http.StatusBadRequest, "bad_request")
				return
			}
		}
		switch r.URL.Path {
		case "/v1/inspect":
			p.inspected.Add(1)
		case "/v1/place":
			p.placed.Add(1)
			p.mu.Lock()
			p.places = append(p.places, params)
			p.mu.Unlock()
		case "/v1/render":
			p.rendered.Add(1)
			p.mu.Lock()
			p.renderParams = append(p.renderParams, params)
			p.mu.Unlock()
		default:
			problem(http.StatusNotFound, "not_found")
			return
		}
		if p.down.Load() {
			problem(http.StatusServiceUnavailable, "busy")
			return
		}
		if p.status != 0 {
			problem(p.status, p.code)
			return
		}
		if len(pdf) < 5 || string(pdf[:5]) != "%PDF-" {
			problem(http.StatusUnprocessableEntity, "pdf_unreadable")
			return
		}
		switch r.URL.Path {
		case "/v1/inspect":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"pages": p.pages, "page": map[string]any{"widthPt": p.widthPt, "heightPt": p.heightPt, "rotate": 0},
				"encrypted": p.encrypted, "textCount": p.textCount, "truncated": false, "spans": []any{}, "runs": []any{},
			})
		case "/v1/place":
			rect, _ := params["rect"].(map[string]any)
			if _, ok := params["pageWidthPt"].(float64); !ok || rect == nil {
				problem(http.StatusBadRequest, "bad_request")
				return
			}
			w.Header().Set("Content-Type", "application/pdf")
			answer := []byte(placedPDF)
			if p.placeAnswer != nil {
				answer = p.placeAnswer
			}
			_, _ = w.Write(answer)
		case "/v1/render":
			w.Header().Set("Content-Type", "image/png")
			answer := append([]byte("\x89PNG\r\n\x1a\n"), "rendered"...)
			if p.renderAnswer != nil {
				answer = p.renderAnswer
			}
			_, _ = w.Write(answer)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (p *pdfToolsContract) client(t *testing.T) *certificate.PDFToolsClient {
	srv := p.server(t)
	return &certificate.PDFToolsClient{BaseURL: srv.URL, HTTP: srv.Client(), MaxBusyWait: time.Millisecond}
}

func TestNewPDFToolsIsNilWithoutAnAddress(t *testing.T) {
	t.Parallel()
	if certificate.NewPDFTools("") != nil || certificate.NewPDFTools("  ") != nil {
		t.Fatal("an empty PDF_TOOLS_URL must leave pdf-tools off (nil)")
	}
	if certificate.NewPDFTools("http://pdf-tools:8080") == nil {
		t.Fatal("an address must give a client")
	}
}

func TestPDFToolsClientSpeaksTheContract(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	contract := &pdfToolsContract{widthPt: 595.276, heightPt: 841.89, pages: 1, textCount: 7}
	client := contract.client(t)
	info, err := client.Inspect(ctx, onePagePDF())
	if err != nil || info != (certificate.PDFInfo{Pages: 1, WidthPt: 595.276, HeightPt: 841.89, TextCount: 7}) {
		t.Fatalf("inspect: %+v %v", info, err)
	}
	placed, err := client.Place(ctx, onePagePDF(), certificate.PDFPage{WidthPt: 842.25, HeightPt: 595.5}, certificate.PDFRect{X: -10, Y: 0, W: 421, H: 595.5})
	if err != nil || string(placed) != placedPDF {
		t.Fatalf("place: %q %v", placed, err)
	}
	want := map[string]any{"pageWidthPt": 842.25, "pageHeightPt": 595.5, "rect": map[string]any{"x": -10.0, "y": 0.0, "w": 421.0, "h": 595.5}}
	if got, _ := json.Marshal(contract.places[0]); string(got) != mustJSON(t, want) {
		t.Fatalf("place params %s, want %s", got, mustJSON(t, want))
	}
	png, err := client.Render(ctx, onePagePDF(), 1200)
	if err != nil || string(png[:4]) != "\x89PNG" || contract.renderParams[0]["maxPx"] != 1200.0 {
		t.Fatalf("render: %q %v %v", png, err, contract.renderParams)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A failure on pdf-tools' side is 503 pdf_tools_unavailable, tried again
// by the caller, never ErrInvalid; a busy pdf-tools (503) gets one more
// try and no more, so a call never loops.
func TestPDFToolsClientUnavailable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		contract *pdfToolsContract
		calls    int32
	}{
		{"busy twice", &pdfToolsContract{pages: 1, status: http.StatusServiceUnavailable, code: "busy", retryAfter: "5"}, 2},
		{"internal error", &pdfToolsContract{pages: 1, status: http.StatusInternalServerError, code: "internal"}, 1},
		{"place answers no PDF", &pdfToolsContract{pages: 1, placeAnswer: []byte("<html>proxy</html>")}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := tc.contract.client(t).Place(context.Background(), onePagePDF(), certificate.PDFPage{WidthPt: 842, HeightPt: 595}, certificate.PDFRect{W: 842, H: 595})
			assertPDFToolsUnavailable(t, err)
			if got := tc.contract.calls(); got != tc.calls {
				t.Fatalf("%d calls, want %d", got, tc.calls)
			}
		})
	}
	t.Run("render answers no PNG", func(t *testing.T) {
		t.Parallel()
		contract := &pdfToolsContract{pages: 1, renderAnswer: []byte("GIF89a")}
		_, err := contract.client(t).Render(context.Background(), onePagePDF(), 2000)
		assertPDFToolsUnavailable(t, err)
	})
	t.Run("a pdf-tools without the route", func(t *testing.T) {
		t.Parallel()
		// An image older than /v1/font/sanitize answers 404: pdf-tools'
		// side, not the font's.
		_, err := (&pdfToolsContract{pages: 1}).client(t).SanitizeFont(context.Background(), []byte("OTTO font"))
		assertPDFToolsUnavailable(t, err)
	})
	t.Run("unreachable", func(t *testing.T) {
		t.Parallel()
		srv := (&pdfToolsContract{}).server(t)
		srv.Close()
		_, err := (&certificate.PDFToolsClient{BaseURL: srv.URL}).Inspect(context.Background(), onePagePDF())
		assertPDFToolsUnavailable(t, err)
	})
	t.Run("nil", func(t *testing.T) {
		t.Parallel()
		var client *certificate.PDFToolsClient
		_, err := client.Inspect(context.Background(), onePagePDF())
		assertPDFToolsUnavailable(t, err)
	})
}

func assertPDFToolsUnavailable(t *testing.T, err error) {
	t.Helper()
	var unavailable *certificate.UnavailableError
	if !errors.As(err, &unavailable) || unavailable.Code != certificate.CodePDFToolsUnavailable {
		t.Fatalf("err = %v, want %s", err, certificate.CodePDFToolsUnavailable)
	}
	if errors.Is(err, certificate.ErrInvalid) {
		t.Fatal("an unavailable pdf-tools must not read as an invalid template")
	}
}

// What pdf-tools refuses of the PDF is said by core's code; a refusal of
// core's own request is render_failed. Neither is tried again.
func TestPDFToolsClientRefusalCodes(t *testing.T) {
	t.Parallel()
	for code, want := range map[string]string{
		"pdf_encrypted":  certificate.CodeBackgroundPDFEncrypted,
		"pdf_pages":      certificate.CodeBackgroundPDFPages,
		"pdf_unreadable": certificate.CodeBackgroundPDFUnreadable,
		"pdf_timeout":    certificate.CodeBackgroundPDFUnreadable,
		"pdf_too_large":  certificate.CodeBackgroundPDFTooLarge,
		"bad_request":    certificate.CodeRenderFailed,
	} {
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			status := http.StatusUnprocessableEntity
			if code == "bad_request" {
				status = http.StatusBadRequest
			}
			contract := &pdfToolsContract{pages: 1, status: status, code: code}
			_, err := contract.client(t).Inspect(context.Background(), onePagePDF())
			var refused *certificate.InvalidError
			if !errors.As(err, &refused) || refused.Code != want || contract.calls() != 1 {
				t.Fatalf("err = %v (calls %d), want %s", err, contract.calls(), want)
			}
		})
	}
}

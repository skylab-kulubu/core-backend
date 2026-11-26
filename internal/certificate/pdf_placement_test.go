package certificate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/media"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// mapArtifacts is an ArtifactStore over a map.
type mapArtifacts map[string][]byte

func (m mapArtifacts) Put(_ context.Context, key string, data []byte, _ media.BlobMetadata) error {
	m[key] = data
	return nil
}

func (m mapArtifacts) Read(_ context.Context, key string) ([]byte, error) {
	data, ok := m[key]
	if !ok {
		return nil, media.ErrNotFound
	}
	return data, nil
}

type placeCall struct {
	page PDFPage
	rect PDFRect
}

// recordingPDFTools answers like pdf-tools and records what it was asked.
type recordingPDFTools struct {
	mu       sync.Mutex
	info     PDFInfo
	placed   []byte
	err      error
	inspects int
	places   []placeCall
	renders  int
}

func (r *recordingPDFTools) Inspect(context.Context, []byte) (PDFInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inspects++
	return r.info, r.err
}

func (r *recordingPDFTools) Place(_ context.Context, _ []byte, page PDFPage, rect PDFRect) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.places = append(r.places, placeCall{page, rect})
	return r.placed, r.err
}

func (r *recordingPDFTools) Render(context.Context, []byte, int) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.renders++
	return append(append([]byte{}, pngSignature...), "IHDR"...), r.err
}

func (r *recordingPDFTools) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inspects + len(r.places) + r.renders
}

// The box core sends to pdf-tools' /v1/place is the golden table's box
// (PlaceBackground), in pt, on a page of the layout's size in pt: what the
// panel's editor draws is what is printed. Checked on the JSON body
// pdf-tools reads.
func TestPlaceSendsTheGoldenTableBox(t *testing.T) {
	t.Parallel()
	tolerance, cases := placementTable(t)
	var mu sync.Mutex
	var sent []map[string]any
	sources := map[string][2]float64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength <= 0 {
			http.Error(w, "length required", http.StatusLengthRequired)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "file", http.StatusBadRequest)
			return
		}
		pdf, _ := io.ReadAll(file)
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/v1/inspect":
			size := sources[string(pdf)]
			_ = json.NewEncoder(w).Encode(map[string]any{"pages": 1, "page": map[string]any{"widthPt": size[0], "heightPt": size[1], "rotate": 0}, "textCount": 0})
		case "/v1/place":
			var params map[string]any
			if err := json.Unmarshal([]byte(r.FormValue("params")), &params); err != nil {
				http.Error(w, "params", http.StatusBadRequest)
				return
			}
			sent = append(sent, params)
			_, _ = io.WriteString(w, "%PDF-1.7 placed")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s := &service{pdfTools: &PDFToolsClient{BaseURL: srv.URL, HTTP: srv.Client()}, pdfCache: newPDFCaches()}
	id := legacyBackgroundID
	for i, c := range cases {
		// Each case its own PDF, so the page cache does not answer for another.
		pdf := []byte("%PDF-1.7 case " + strconv.Itoa(i))
		mu.Lock()
		sources[string(pdf)] = [2]float64{c.Source.Width * 0.75, c.Source.Height * 0.75}
		mu.Unlock()
		placement := c.Placement
		layout := Layout{Width: c.Page.Width, Height: c.Page.Height, BackgroundMediaID: &id, BackgroundPlacement: &placement}
		if _, err := s.placePDF(t.Context(), layout, pdf); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		mu.Lock()
		params := sent[len(sent)-1]
		mu.Unlock()
		rect, _ := params["rect"].(map[string]any)
		num := func(v any) float64 { f, _ := v.(float64); return f * 96 / 72 }
		got := Rect{Left: num(rect["x"]), Top: num(rect["y"]), Width: num(rect["w"]), Height: num(rect["h"])}
		if !sameRect(got, c.Want, tolerance) || !near(num(params["pageWidthPt"]), c.Page.Width, 1e-9) || !near(num(params["pageHeightPt"]), c.Page.Height, 1e-9) {
			t.Fatalf("%s: sent %v (box %+v px), want %+v on %gx%g", c.Name, params, got, c.Want, c.Page.Width, c.Page.Height)
		}
	}
	if len(sent) != len(cases) {
		t.Fatalf("%d places for %d cases", len(sent), len(cases))
	}
}

// The same PDF, page and box is placed once; the cache keeps at most its
// entries and bytes.
func TestPlacedPDFCache(t *testing.T) {
	t.Parallel()
	tools := &recordingPDFTools{info: PDFInfo{Pages: 1, WidthPt: 842, HeightPt: 595}, placed: []byte("%PDF-1.7 placed")}
	s := &service{pdfTools: tools, pdfCache: newPDFCaches()}
	id := legacyBackgroundID
	layout := Layout{Width: 1123, Height: 794, BackgroundMediaID: &id, BackgroundPlacement: &BackgroundPlacement{Fit: FitCover, X: 0.5, Y: 0.5, Scale: 1}}
	for range 3 {
		if _, err := s.placePDF(t.Context(), layout, []byte("%PDF-1.7 one")); err != nil {
			t.Fatal(err)
		}
	}
	if tools.inspects != 1 || len(tools.places) != 1 {
		t.Fatalf("inspects %d places %d, want 1 and 1", tools.inspects, len(tools.places))
	}
	layout.BackgroundPlacement = &BackgroundPlacement{Fit: FitContain, X: 0.5, Y: 0.5, Scale: 1}
	if _, err := s.placePDF(t.Context(), layout, []byte("%PDF-1.7 one")); err != nil || len(tools.places) != 2 {
		t.Fatalf("another box: %v, places %d", err, len(tools.places))
	}

	c := newLRU(3, 10, func(b []byte) int { return len(b) })
	for i := range 5 {
		c.put(fmt.Sprint(i), []byte("ab"))
	}
	if c.len() != 3 {
		t.Fatalf("entries %d, want 3", c.len())
	}
	if _, ok := c.get("0"); ok {
		t.Fatal("the oldest entry stayed")
	}
	c.put("big", []byte("0123456789"))
	if c.len() != 1 || c.bytes != 10 {
		t.Fatalf("entries %d bytes %d after a 10-byte entry", c.len(), c.bytes)
	}
	c.put("huge", []byte("0123456789x"))
	if _, ok := c.get("huge"); ok {
		t.Fatal("an entry over the byte bound was kept")
	}
}

// A PDF pdf-tools says is encrypted or of several pages is refused by code
// before it is placed; a failure on pdf-tools' side is counted by
// operation.
func TestPlacePDFRefusesAndCounts(t *testing.T) {
	t.Parallel()
	id := legacyBackgroundID
	layout := Layout{Width: 1123, Height: 794, BackgroundMediaID: &id, BackgroundPlacement: &BackgroundPlacement{Fit: FitCover, X: 0.5, Y: 0.5, Scale: 1}}
	for _, tc := range []struct {
		info PDFInfo
		code string
	}{
		{PDFInfo{Pages: 1, WidthPt: 842, HeightPt: 595, Encrypted: true}, CodeBackgroundPDFEncrypted},
		{PDFInfo{Pages: 2, WidthPt: 842, HeightPt: 595}, CodeBackgroundPDFPages},
	} {
		tools := &recordingPDFTools{info: tc.info}
		_, err := (&service{pdfTools: tools, pdfCache: newPDFCaches()}).placePDF(t.Context(), layout, []byte("%PDF-1.7"))
		var refused *InvalidError
		if !errors.As(err, &refused) || refused.Code != tc.code || len(tools.places) != 0 {
			t.Fatalf("%+v: %v, places %d", tc.info, err, len(tools.places))
		}
	}

	s := &service{pdfTools: &recordingPDFTools{err: pdfToolsUnavailable(nil)}, pdfCache: newPDFCaches()}
	if _, err := s.placePDF(t.Context(), layout, []byte("%PDF-1.7")); err == nil {
		t.Fatal("no error")
	}
	// A request its caller cancelled is not pdf-tools' failure.
	cancelled := &service{pdfTools: &recordingPDFTools{err: pdfToolsUnavailable(fmt.Errorf("post: %w", context.Canceled))}, pdfCache: newPDFCaches()}
	if _, err := cancelled.placePDF(t.Context(), layout, []byte("%PDF-1.7")); err == nil {
		t.Fatal("no error")
	}
	if got := cancelled.pdfToolsErrors.prometheus(); !strings.Contains(got, `core_certificate_pdf_tools_errors_total{op="inspect"} 0`) {
		t.Fatalf("a cancelled call was counted:\n%s", got)
	}
	if got := s.pdfToolsErrors.prometheus(); !strings.Contains(got, `core_certificate_pdf_tools_errors_total{op="inspect"} 1`) ||
		!strings.Contains(got, `core_certificate_pdf_tools_errors_total{op="place"} 0`) {
		t.Fatalf("metrics:\n%s", got)
	}
}

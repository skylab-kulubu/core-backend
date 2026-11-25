package certificate

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

// The bounds of the in-process caches of pdf-tools' answers: the placed
// PDFs a draft or Event preview draws (SPEC §4.2), and each PDF's page.
const (
	placedCacheEntries = 64
	placedCacheBytes   = 200 << 20
	infoCacheEntries   = 256
	// maxBackgroundPDFBytes is the largest PDF background: the catalog's
	// and pdf-tools' bound.
	maxBackgroundPDFBytes = 20 << 20
	// pxPerPt turns a PDF's pt into layout px (CSS: 96 px, 72 pt an inch).
	pxPerPt = 96.0 / 72
)

// The pdf-tools operations core_certificate_pdf_tools_errors_total counts.
const (
	pdfOpInspect = "inspect"
	pdfOpPlace   = "place"
	pdfOpRender  = "render"
)

// pdfToolsMetrics counts the calls to pdf-tools that failed on its side
// (pdf_tools_unavailable), by operation. A PDF it refuses is not counted.
type pdfToolsMetrics struct {
	inspect, place, render atomic.Int64
}

func (m *pdfToolsMetrics) add(op string) {
	switch op {
	case pdfOpInspect:
		m.inspect.Add(1)
	case pdfOpPlace:
		m.place.Add(1)
	case pdfOpRender:
		m.render.Add(1)
	}
}

func (m *pdfToolsMetrics) prometheus() string {
	return "# TYPE core_certificate_pdf_tools_errors_total counter\n" +
		fmt.Sprintf("core_certificate_pdf_tools_errors_total{op=%q} %d\n", pdfOpInspect, m.inspect.Load()) +
		fmt.Sprintf("core_certificate_pdf_tools_errors_total{op=%q} %d\n", pdfOpPlace, m.place.Load()) +
		fmt.Sprintf("core_certificate_pdf_tools_errors_total{op=%q} %d\n", pdfOpRender, m.render.Load())
}

// lru is a least recently used cache bounded by entries and by bytes.
type lru[V any] struct {
	mu         sync.Mutex
	maxEntries int
	maxBytes   int
	size       func(V) int
	bytes      int
	order      *list.List
	items      map[string]*list.Element
}

type lruEntry[V any] struct {
	key   string
	value V
}

func newLRU[V any](maxEntries, maxBytes int, size func(V) int) *lru[V] {
	return &lru[V]{maxEntries: maxEntries, maxBytes: maxBytes, size: size, order: list.New(), items: map[string]*list.Element{}}
}

func (c *lru[V]) get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		return el.Value.(*lruEntry[V]).value, true
	}
	var zero V
	return zero, false
}

func (c *lru[V]) put(key string, value V) {
	n := c.size(value)
	if n > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.bytes -= c.size(el.Value.(*lruEntry[V]).value)
		c.order.Remove(el)
		delete(c.items, key)
	}
	c.items[key] = c.order.PushFront(&lruEntry[V]{key: key, value: value})
	c.bytes += n
	for c.order.Len() > c.maxEntries || c.bytes > c.maxBytes {
		oldest := c.order.Back()
		entry := oldest.Value.(*lruEntry[V])
		c.bytes -= c.size(entry.value)
		c.order.Remove(oldest)
		delete(c.items, entry.key)
	}
}

func (c *lru[V]) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// pdfCaches are a service's caches of pdf-tools' answers.
type pdfCaches struct {
	info   *lru[PDFInfo]
	placed *lru[[]byte]
}

func newPDFCaches() pdfCaches {
	return pdfCaches{
		info:   newLRU(infoCacheEntries, infoCacheEntries, func(PDFInfo) int { return 1 }),
		placed: newLRU(placedCacheEntries, placedCacheBytes, func(b []byte) int { return len(b) }),
	}
}

func isPDF(contentType string) bool {
	return strings.EqualFold(strings.TrimSpace(strings.Split(contentType, ";")[0]), "application/pdf")
}

// checkBackgroundPDF refuses what is no PDF background before anything
// reads it.
func checkBackgroundPDF(data []byte) error {
	switch {
	case len(data) > maxBackgroundPDFBytes:
		return invalid(CodeBackgroundPDFTooLarge)
	case !bytes.Contains(data[:min(len(data), 1024)], []byte("%PDF-")):
		return invalid(CodeBackgroundPDFUnreadable)
	}
	return nil
}

// countPDFToolsError counts err when pdf-tools failed on its side.
func (s *service) countPDFToolsError(op string, err error) {
	var unavailable *UnavailableError
	if errors.As(err, &unavailable) && unavailable.Code == CodePDFToolsUnavailable {
		s.pdfToolsErrors.add(op)
	}
}

// pdfInfo is the PDF's page as pdf-tools reads it, cached by the PDF's
// hash.
func (s *service) pdfInfo(ctx context.Context, pdf []byte) (PDFInfo, error) {
	if s.pdfTools == nil {
		return PDFInfo{}, pdfToolsUnavailable(nil)
	}
	if err := checkBackgroundPDF(pdf); err != nil {
		return PDFInfo{}, err
	}
	sum := sha256.Sum256(pdf)
	key := hex.EncodeToString(sum[:])
	if info, ok := s.pdfCache.info.get(key); ok {
		return info, nil
	}
	info, err := s.pdfTools.Inspect(ctx, pdf)
	if err != nil {
		s.countPDFToolsError(pdfOpInspect, err)
		return PDFInfo{}, err
	}
	s.pdfCache.info.put(key, info)
	return info, nil
}

// placePDF is the PDF background placed on the layout's page by its
// placement: a one-page PDF of the page's size with the source page drawn
// into PlaceBackground's box and cut to the page, which Gotenberg lays
// under the page at scale 1 (the same size, so exactly). It is cached by
// the PDF's hash, the page and the box.
func (s *service) placePDF(ctx context.Context, layout Layout, pdf []byte) ([]byte, error) {
	if layout.BackgroundPlacement == nil {
		return nil, invalid(CodeLayoutPlacementInvalid)
	}
	info, err := s.pdfInfo(ctx, pdf)
	if err != nil {
		return nil, err
	}
	switch {
	case info.Encrypted:
		return nil, invalid(CodeBackgroundPDFEncrypted)
	case info.Pages != 1:
		return nil, invalid(CodeBackgroundPDFPages)
	}
	box := PlaceBackground(layout.Width, layout.Height, info.WidthPt*pxPerPt, info.HeightPt*pxPerPt, *layout.BackgroundPlacement)
	page := PDFPage{WidthPt: layout.Width / pxPerPt, HeightPt: layout.Height / pxPerPt}
	rect := PDFRect{X: box.Left / pxPerPt, Y: box.Top / pxPerPt, W: box.Width / pxPerPt, H: box.Height / pxPerPt}
	if !finite(page.WidthPt, page.HeightPt, rect.X, rect.Y, rect.W, rect.H) || rect.W <= 0 || rect.H <= 0 {
		return nil, invalid(CodeLayoutPlacementInvalid)
	}
	sum := sha256.Sum256(pdf)
	key := fmt.Sprintf("%x|%g|%g|%g|%g|%g|%g", sum, page.WidthPt, page.HeightPt, rect.X, rect.Y, rect.W, rect.H)
	if placed, ok := s.pdfCache.placed.get(key); ok {
		return placed, nil
	}
	placed, err := s.pdfTools.Place(ctx, pdf, page, rect)
	if err != nil {
		s.countPDFToolsError(pdfOpPlace, err)
		return nil, err
	}
	if len(placed) > maxRenderedPDFBytes || !bytes.HasPrefix(placed, []byte("%PDF-")) {
		s.pdfToolsErrors.add(pdfOpPlace)
		return nil, pdfToolsUnavailable(errors.New("pdf-tools: place answered no PDF"))
	}
	s.pdfCache.placed.put(key, placed)
	return placed, nil
}

// placesPDFBackground is whether the layout's PDF background (contentType)
// is placed by pdf-tools: it has a placement. A layout without one prints
// the PDF as before, without pdf-tools.
func placesPDFBackground(layout Layout, contentType string) bool {
	return layout.BackgroundMediaID != nil && layout.BackgroundPlacement != nil && isPDF(contentType)
}

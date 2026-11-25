package certificate

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// PDFTools is core's internal PDF service (pdf-tools/README.md): it reads a
// background PDF's page, places its page into a box on a new page and
// renders it to PNG. PDFToolsClient also checks fonts (FontSanitizer). Core finds it at PDF_TOOLS_URL; nil while that is
// unset, and then a placed PDF background, the asset info of a PDF and its
// preview answer 503 pdf_tools_unavailable while the rest works as before.
type PDFTools interface {
	Inspect(ctx context.Context, pdf []byte) (PDFInfo, error)
	Place(ctx context.Context, pdf []byte, page PDFPage, rect PDFRect) ([]byte, error)
	Render(ctx context.Context, pdf []byte, maxPx int) ([]byte, error)
	// Text is what /v1/inspect says of the PDF's text: every span, its
	// place, font and style, and how pdf-tools groups them.
	Text(ctx context.Context, pdf []byte) (PDFText, error)
	// StripText is the PDF without the spans named (/v1/strip-text).
	StripText(ctx context.Context, pdf []byte, spanIDs []string) ([]byte, error)
}

// PDFInfo is what /v1/inspect says of a PDF: its page count and its first
// page's visible box (CropBox, turned by /Rotate) in pt.
type PDFInfo struct {
	Pages     int
	WidthPt   float64
	HeightPt  float64
	Rotate    int
	Encrypted bool
	TextCount int
}

// PDFPage is a page's size in pt.
type PDFPage struct {
	WidthPt  float64 `json:"pageWidthPt"`
	HeightPt float64 `json:"pageHeightPt"`
}

// PDFRect is a box on a page in pt, origin top left, y down.
type PDFRect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// Bounds on what core takes back from pdf-tools: it trusts the answer no
// more than the upload (SPEC §6.3).
const (
	maxPDFToolsJSONBytes = 16 << 20
	maxPDFToolsPNGBytes  = 32 << 20
	// pdfToolsAttempts bounds the tries of one call: a 503 (every worker
	// busy) is tried once more after a short wait, then the call fails
	// with pdf_tools_unavailable; it never loops.
	pdfToolsAttempts = 2
	pdfToolsMaxWait  = 2 * time.Second
)

// pngSignature opens every PNG file.
var pngSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// PDFToolsClient calls pdf-tools over HTTP.
type PDFToolsClient struct {
	BaseURL string
	HTTP    *http.Client
	// MaxBusyWait bounds the wait before the one more try a busy answer
	// gets; 0 is two seconds.
	MaxBusyWait time.Duration
}

// NewPDFTools is the client of the pdf-tools at baseURL, nil when it is
// empty (the features that need it stay off).
func NewPDFTools(baseURL string) PDFTools {
	if strings.TrimSpace(baseURL) == "" {
		return nil
	}
	return &PDFToolsClient{BaseURL: strings.TrimSpace(baseURL)}
}

// pdfToolsUnavailable is pdf-tools missing, unreachable or failing on its
// side (a 5xx): a later try may pass.
func pdfToolsUnavailable(cause error) error {
	return &UnavailableError{Code: CodePDFToolsUnavailable, Err: cause}
}

func (c *PDFToolsClient) Inspect(ctx context.Context, pdf []byte) (PDFInfo, error) {
	body, err := c.call(ctx, "/v1/inspect", pdf, nil, "application/json", maxPDFToolsJSONBytes)
	if err != nil {
		return PDFInfo{}, err
	}
	var answer struct {
		Pages int `json:"pages"`
		Page  struct {
			WidthPt  float64 `json:"widthPt"`
			HeightPt float64 `json:"heightPt"`
			Rotate   int     `json:"rotate"`
		} `json:"page"`
		Encrypted bool `json:"encrypted"`
		TextCount int  `json:"textCount"`
	}
	if err := json.Unmarshal(body, &answer); err != nil || answer.Pages < 1 || !finite(answer.Page.WidthPt, answer.Page.HeightPt) ||
		answer.Page.WidthPt <= 0 || answer.Page.HeightPt <= 0 || answer.TextCount < 0 {
		return PDFInfo{}, pdfToolsUnavailable(errors.New("pdf-tools: inspect answered no page"))
	}
	return PDFInfo{
		Pages: answer.Pages, WidthPt: answer.Page.WidthPt, HeightPt: answer.Page.HeightPt, Rotate: answer.Page.Rotate,
		Encrypted: answer.Encrypted, TextCount: answer.TextCount,
	}, nil
}

func (c *PDFToolsClient) Text(ctx context.Context, pdf []byte) (PDFText, error) {
	body, err := c.call(ctx, "/v1/inspect", pdf, nil, "application/json", maxPDFToolsJSONBytes)
	if err != nil {
		return PDFText{}, err
	}
	return parsePDFText(body)
}

func (c *PDFToolsClient) StripText(ctx context.Context, pdf []byte, spanIDs []string) ([]byte, error) {
	body, err := c.call(ctx, "/v1/strip-text", pdf, map[string][]string{"spanIds": spanIDs}, "application/pdf", maxBackgroundPDFBytes)
	if err != nil {
		return nil, stripRefusal(err)
	}
	if !bytes.HasPrefix(body, []byte("%PDF-")) {
		return nil, pdfToolsUnavailable(errors.New("pdf-tools: strip-text answered no PDF"))
	}
	return body, nil
}

// SanitizeFont checks a font with pdf-tools (/v1/font/sanitize): OTS, then
// fontTools; the answer is the font's facts and the font as WOFF2. A font
// pdf-tools refuses is one of the font codes (pdfToolsRefusal).
func (c *PDFToolsClient) SanitizeFont(ctx context.Context, font []byte) (SanitizedFont, error) {
	body, err := c.call(ctx, "/v1/font/sanitize", font, nil, "application/json", maxPDFToolsJSONBytes)
	if err != nil {
		return SanitizedFont{}, err
	}
	var answer struct {
		SanitizedFont
		WOFF2 string `json:"woff2"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return SanitizedFont{}, pdfToolsUnavailable(errors.New("pdf-tools: font/sanitize answered no JSON"))
	}
	woff2, err := base64.StdEncoding.DecodeString(answer.WOFF2)
	if err != nil || !bytes.HasPrefix(woff2, woff2Signature) {
		return SanitizedFont{}, pdfToolsUnavailable(errors.New("pdf-tools: font/sanitize answered no WOFF2"))
	}
	out := answer.SanitizedFont
	out.WOFF2 = woff2
	return out, nil
}

func (c *PDFToolsClient) Place(ctx context.Context, pdf []byte, page PDFPage, rect PDFRect) ([]byte, error) {
	params := struct {
		PDFPage
		Rect PDFRect `json:"rect"`
	}{page, rect}
	body, err := c.call(ctx, "/v1/place", pdf, params, "application/pdf", maxRenderedPDFBytes)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(body, []byte("%PDF-")) {
		return nil, pdfToolsUnavailable(errors.New("pdf-tools: place answered no PDF"))
	}
	return body, nil
}

func (c *PDFToolsClient) Render(ctx context.Context, pdf []byte, maxPx int) ([]byte, error) {
	body, err := c.call(ctx, "/v1/render", pdf, map[string]int{"maxPx": maxPx}, "image/png", maxPDFToolsPNGBytes)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(body, pngSignature) {
		return nil, pdfToolsUnavailable(errors.New("pdf-tools: render answered no PNG"))
	}
	return body, nil
}

// call posts the PDF (and params) to path and returns the answer's body,
// at most limit bytes. A 5xx, a network error or an answer over limit is
// pdf_tools_unavailable; a 4xx is the PDF's, said by a core code.
func (c *PDFToolsClient) call(ctx context.Context, path string, pdf []byte, params any, accept string, limit int) ([]byte, error) {
	if c == nil || strings.TrimSpace(c.BaseURL) == "" {
		return nil, pdfToolsUnavailable(nil)
	}
	form, contentType, err := pdfToolsForm(pdf, params)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 1; attempt <= pdfToolsAttempts; attempt++ {
		body, retryAfter, err := c.post(ctx, path, form, contentType, accept, limit)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if retryAfter <= 0 || attempt == pdfToolsAttempts {
			break
		}
		if err := c.sleep(ctx, retryAfter); err != nil {
			break
		}
	}
	return nil, lastErr
}

// post makes one request. retryAfter is above zero only for a busy answer
// (503) worth one more try.
func (c *PDFToolsClient) post(ctx context.Context, path string, form []byte, contentType, accept string, limit int) (_ []byte, retryAfter time.Duration, _ error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(form))
	if err != nil {
		return nil, 0, pdfToolsUnavailable(err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", accept+", application/problem+json")
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, 0, pdfToolsUnavailable(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return nil, 0, pdfToolsUnavailable(err)
	}
	switch {
	case resp.StatusCode == http.StatusServiceUnavailable:
		return nil, c.busyWait(resp.Header.Get("Retry-After")), pdfToolsUnavailable(fmt.Errorf("pdf-tools: HTTP %d", resp.StatusCode))
	case resp.StatusCode >= 500:
		return nil, 0, pdfToolsUnavailable(fmt.Errorf("pdf-tools: HTTP %d", resp.StatusCode))
	case resp.StatusCode >= 400:
		return nil, 0, pdfToolsRefusal(body)
	case resp.StatusCode != http.StatusOK || len(body) == 0 || len(body) > limit:
		return nil, 0, pdfToolsUnavailable(fmt.Errorf("pdf-tools: HTTP %d, %d bytes", resp.StatusCode, len(body)))
	}
	return body, 0, nil
}

// pdfToolsRefusal is a 4xx in core's words. pdf-tools refuses only what
// the PDF is (the same request fails again); anything else it refuses is a
// request core should not have made, which a retry does not change either.
func pdfToolsRefusal(body []byte) error {
	var problem struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &problem)
	switch problem.Code {
	case "strip_incomplete":
		return invalid(CodeBackgroundTextStripFailed)
	case "pdf_encrypted":
		return invalid(CodeBackgroundPDFEncrypted)
	case "pdf_pages":
		return invalid(CodeBackgroundPDFPages)
	case "pdf_too_large":
		return invalid(CodeBackgroundPDFTooLarge)
	case "pdf_unreadable", "pdf_timeout":
		return invalid(CodeBackgroundPDFUnreadable)
	// A font's (/v1/font/sanitize; docs/certificate-fonts.md).
	case "font_unreadable", "font_timeout":
		return invalid(CodeFontUnreadable)
	case "font_too_large":
		return invalid(CodeFontTooLarge)
	case "font_variable", "font_collection":
		return invalid(CodeFontUnsupported)
	case "font_embedding_restricted":
		return invalid(CodeFontEmbeddingRestricted)
	default:
		return invalid(CodeRenderFailed)
	}
}

// stripRefusal is a request /v1/strip-text refused (400 bad_request:
// span ids it does not know, or cannot remove) in core's words: the spans
// chosen cannot be converted.
func stripRefusal(err error) error {
	var refused *InvalidError
	if errors.As(err, &refused) && refused.Code == CodeRenderFailed {
		return invalid(CodeBackgroundTextSelectionEmpty)
	}
	return err
}

// busyWait is how long to wait before the one more try a busy pdf-tools
// gets: its Retry-After, at most MaxBusyWait.
func (c *PDFToolsClient) busyWait(header string) time.Duration {
	bound := c.MaxBusyWait
	if bound <= 0 {
		bound = pdfToolsMaxWait
	}
	seconds, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || seconds <= 0 {
		return bound
	}
	return min(time.Duration(seconds)*time.Second, bound)
}

func pdfToolsForm(pdf []byte, params any) ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", "background.pdf")
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(pdf); err != nil {
		return nil, "", err
	}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, "", err
		}
		if err := w.WriteField("params", string(raw)); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

func (c *PDFToolsClient) sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *PDFToolsClient) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	// A job runs at most 15 s and waits at most 10 s for a worker.
	return &http.Client{Timeout: 30 * time.Second}
}

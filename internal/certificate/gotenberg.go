package certificate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxRenderedPDFBytes = 50 << 20

type Gotenberg struct {
	BaseURL      string
	HTTP         *http.Client
	validatedPDF sync.Map
}

func (g *Gotenberg) PDF(ctx context.Context, html string) ([]byte, error) {
	return g.convertHTML(ctx, html, nil)
}

func (g *Gotenberg) PDFWithBackground(ctx context.Context, html string, background []byte) ([]byte, error) {
	if g == nil || strings.TrimSpace(g.BaseURL) == "" || len(background) == 0 || len(background) > maxRenderedPDFBytes || !bytes.Contains(background[:min(len(background), 1024)], []byte("%PDF-")) {
		return nil, ErrInvalid
	}
	hash := sha256.Sum256(background)
	if _, ok := g.validatedPDF.Load(hash); !ok {
		if err := g.validateSinglePagePDF(ctx, background); err != nil {
			return nil, err
		}
		g.validatedPDF.Store(hash, struct{}{})
	}
	return g.convertHTML(ctx, html, background)
}

func (g *Gotenberg) convertHTML(ctx context.Context, html string, background []byte) ([]byte, error) {
	if g == nil || strings.TrimSpace(g.BaseURL) == "" {
		return nil, ErrInvalid
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("files", "index.html")
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(part, html); err != nil {
		return nil, err
	}
	for key, value := range map[string]string{
		"printBackground":   "true",
		"preferCssPageSize": "true",
	} {
		if err := w.WriteField(key, value); err != nil {
			return nil, err
		}
	}
	if len(background) > 0 {
		for key, value := range map[string]string{
			"omitBackground":      "true",
			"watermarkSource":     "pdf",
			"watermarkExpression": "background.pdf",
			"watermarkOptions":    `{"scale":1,"opacity":1}`,
		} {
			if err := w.WriteField(key, value); err != nil {
				return nil, err
			}
		}
		backgroundPart, err := w.CreateFormFile("watermark", "background.pdf")
		if err != nil {
			return nil, err
		}
		if _, err := backgroundPart.Write(background); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(g.BaseURL, "/")+"/forms/chromium/convert/html", &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	client := g.client()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRenderedPDFBytes+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, ErrInvalid
	}
	if len(body) == 0 || len(body) > maxRenderedPDFBytes || !bytes.HasPrefix(body, []byte("%PDF-")) {
		return nil, ErrInvalid
	}
	return body, nil
}

func (g *Gotenberg) validateSinglePagePDF(ctx context.Context, background []byte) error {
	if g == nil || strings.TrimSpace(g.BaseURL) == "" {
		return ErrInvalid
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("files", "background.pdf")
	if err != nil {
		return err
	}
	if _, err := part.Write(background); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(g.BaseURL, "/")+"/forms/pdfengines/metadata/read", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := g.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || resp.StatusCode >= 300 {
		return ErrInvalid
	}
	metadata := map[string]struct {
		PageCount int    `json:"PageCount"`
		MIMEType  string `json:"MIMEType"`
	}{}
	if err := json.Unmarshal(body, &metadata); err != nil {
		return ErrInvalid
	}
	item, ok := metadata["background.pdf"]
	if !ok || item.PageCount != 1 || (item.MIMEType != "" && !strings.EqualFold(item.MIMEType, "application/pdf")) {
		return ErrInvalid
	}
	return nil
}

func (g *Gotenberg) client() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

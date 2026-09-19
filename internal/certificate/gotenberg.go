package certificate

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

const maxRenderedPDFBytes = 50 << 20

type Gotenberg struct {
	BaseURL string
	HTTP    *http.Client
}

func (g *Gotenberg) PDF(ctx context.Context, html string) ([]byte, error) {
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
	if err := w.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(g.BaseURL, "/")+"/forms/chromium/convert/html", &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	client := g.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
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

package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

func (s *SkyMail) Certificate(ctx context.Context, recipientEmail, fullName string, vars map[string]string) {
	if s == nil || s.CertificateTemplateID == uuid.Nil || strings.TrimSpace(s.BaseURL) == "" {
		return
	}
	if strings.TrimSpace(recipientEmail) == "" {
		return
	}
	token, err := s.Tokens.Token(ctx)
	if err != nil || token == "" {
		return
	}
	body, err := json.Marshal(map[string]any{
		"template_id":         s.CertificateTemplateID,
		"recipient_email":     recipientEmail,
		"recipient_full_name": fullName,
		"body_variables":      vars,
	})
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.BaseURL, "/")+"/v1/mail_tasks/single", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := s.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
}

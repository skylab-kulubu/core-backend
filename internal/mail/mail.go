package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type Mailer interface {
	Welcome(ctx context.Context, u user.User)
}

type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

type StaticToken string

func (s StaticToken) Token(context.Context) (string, error) {
	return string(s), nil
}

type SkyMail struct {
	BaseURL               string
	TemplateID            uuid.UUID
	CertificateTemplateID uuid.UUID
	Tokens                TokenSource
	HTTP                  *http.Client
}

func (s *SkyMail) Welcome(ctx context.Context, u user.User) {
	if s == nil || s.TemplateID == uuid.Nil || strings.TrimSpace(s.BaseURL) == "" {
		return
	}
	token, err := s.Tokens.Token(ctx)
	if err != nil || token == "" {
		return
	}
	fullName := strings.TrimSpace(u.FirstName + " " + u.LastName)
	body, err := json.Marshal(map[string]any{
		"template_id":         s.TemplateID,
		"recipient_email":     u.Email,
		"recipient_full_name": fullName,
		"body_variables": map[string]string{
			"FirstName": u.FirstName,
			"LastName":  u.LastName,
			"Email":     u.Email,
			"SkyNumber": u.SkyNumber,
			"CreatedAt": u.CreatedAt.UTC().Format(time.RFC3339),
		},
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

type ClientCredentials struct {
	TokenURL     string
	ClientID     string
	ClientSecret string
	HTTP         *http.Client
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
}

func (c ClientCredentials) Token(ctx context.Context) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", c.ClientID)
	form.Set("client_secret", c.ClientSecret)
	form.Set("scope", "openid")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("token status %d", resp.StatusCode)
	}
	var out tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.AccessToken, nil
}

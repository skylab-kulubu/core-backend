package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

var (
	ErrListNotFound = errors.New("mail: list not found")
	ErrForbidden    = errors.New("mail: lists forbidden — grant skymail:lists:write on core SA")
)

type StatusError struct {
	Method string
	Path   string
	Status int
}

func (e *StatusError) Error() string {
	if e == nil {
		return "mail: empty status"
	}
	if e.Status == http.StatusForbidden {
		return fmt.Sprintf("%s: %s %s status %d", ErrForbidden.Error(), e.Method, e.Path, e.Status)
	}
	return fmt.Sprintf("mail: %s %s status %d", e.Method, e.Path, e.Status)
}

func (e *StatusError) Unwrap() error {
	if e != nil && e.Status == http.StatusForbidden {
		return ErrForbidden
	}
	return nil
}

type ListRecipient struct {
	ID       uuid.UUID `json:"id"`
	FullName string    `json:"full_name"`
	Email    string    `json:"email"`
}

type Lists interface {
	CreateList(ctx context.Context, name string) (uuid.UUID, error)
	DeleteList(ctx context.Context, id uuid.UUID) error
	GetList(ctx context.Context, id uuid.UUID) error
	Recipients(ctx context.Context, id uuid.UUID) ([]ListRecipient, error)
	AddRecipient(ctx context.Context, id uuid.UUID, r ListRecipient) error
	RemoveRecipient(ctx context.Context, listID, recipientID uuid.UUID) error
}

func (s *SkyMail) DeleteList(ctx context.Context, id uuid.UUID) error {
	return s.doJSON(ctx, http.MethodDelete, "/v1/mailing_lists/"+id.String(), nil, http.StatusNoContent, nil)
}

func (s *SkyMail) CreateList(ctx context.Context, name string) (uuid.UUID, error) {
	var out struct {
		ID uuid.UUID `json:"id"`
	}
	if err := s.doJSON(ctx, http.MethodPost, "/v1/mailing_lists", map[string]string{"name": name}, http.StatusCreated, &out); err != nil {
		return uuid.Nil, err
	}
	if out.ID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("mail: empty list id")
	}
	return out.ID, nil
}

func (s *SkyMail) GetList(ctx context.Context, id uuid.UUID) error {
	return s.doJSON(ctx, http.MethodGet, "/v1/mailing_lists/"+id.String(), nil, http.StatusOK, nil)
}

func (s *SkyMail) Recipients(ctx context.Context, id uuid.UUID) ([]ListRecipient, error) {
	var out []ListRecipient
	path := "/v1/mailing_lists/" + id.String() + "/recipients?_start=0&_end=10000"
	if err := s.doJSON(ctx, http.MethodGet, path, nil, http.StatusOK, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []ListRecipient{}
	}
	return out, nil
}

func (s *SkyMail) AddRecipient(ctx context.Context, id uuid.UUID, r ListRecipient) error {
	return s.doJSON(ctx, http.MethodPost, "/v1/mailing_lists/"+id.String()+"/recipients", map[string]string{
		"full_name": r.FullName,
		"email":     r.Email,
	}, http.StatusCreated, nil)
}

func (s *SkyMail) RemoveRecipient(ctx context.Context, listID, recipientID uuid.UUID) error {
	return s.doJSON(ctx, http.MethodDelete, "/v1/mailing_lists/"+listID.String()+"/recipients/"+recipientID.String(), nil, http.StatusNoContent, nil)
}

func (s *SkyMail) doJSON(ctx context.Context, method, path string, body any, want int, dest any) error {
	if s == nil || strings.TrimSpace(s.BaseURL) == "" {
		return errors.New("mail: skymail is not configured")
	}
	token, err := s.Tokens.Token(ctx)
	if err != nil {
		return err
	}
	if token == "" {
		return errors.New("mail: empty token")
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(s.BaseURL, "/")+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := s.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrListNotFound
	}
	if resp.StatusCode != want {
		return &StatusError{Method: method, Path: path, Status: resp.StatusCode}
	}
	if dest == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, dest)
}

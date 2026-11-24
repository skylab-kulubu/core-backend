// Package formrespondents reads a form's respondents from Forms' internal
// route for the Form respondents audience (ADR-0075 karar 6; core
// docs/news-audience.md "Form respondents"):
//
//	GET {FORMS_INTERNAL_URL}/internal/v1/forms/{formId}/respondents?actor={sub}&after=&limit=
//	→ {"actorRole": "owner"|"editor"|"viewer"|"none", "responseCount": n,
//	   "items": [{"userId": "…"} | {"guest": {"name": "…", "email": "…"}}],
//	   "next": "…"|null}
//
// with core's own service token (client core, role
// skyforms:responses:read-service, aud ∋ forms). 404 is a form Forms does
// not know. The route is Forms' (Fatih's) to settle; this is core's side of
// the proposed contract.
package formrespondents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/commpref"
)

// URLEnv is Forms' internal address (http://forms-backend:8080 on the
// platform's network). Read only while the audience is on.
const URLEnv = "FORMS_INTERNAL_URL"

// maxBody bounds one page's answer: 500 respondents with long names.
const maxBody = 1 << 20

// Timeout bounds one page's read.
const Timeout = 15 * time.Second

// Tokens gives core's service token.
type Tokens interface {
	Token(ctx context.Context) (string, error)
}

// Client reads Forms' respondents route.
type Client struct {
	BaseURL string
	Tokens  Tokens
	HTTP    *http.Client
}

// URLFromEnv reads URLEnv: an http(s) origin without query or credentials.
// The error names the variable, never its value.
func URLFromEnv(getenv func(string) string) (string, error) {
	raw := strings.TrimRight(strings.TrimSpace(getenv(URLEnv)), "/")
	if raw == "" {
		return "", fmt.Errorf("%s is required while %s=on", URLEnv, commpref.FormRespondentsEnv)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%s must be an http(s) address without credentials or query", URLEnv)
	}
	return raw, nil
}

type page struct {
	ActorRole     string `json:"actorRole"`
	ResponseCount int    `json:"responseCount"`
	Items         []struct {
		UserID *string `json:"userId"`
		Guest  *struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"guest"`
	} `json:"items"`
	Next *string `json:"next"`
}

// Respondents reads one page.
func (c *Client) Respondents(ctx context.Context, formID, actor uuid.UUID, after string, limit int) (commpref.RespondentsPage, error) {
	token, err := c.Tokens.Token(ctx)
	if err != nil || token == "" {
		return commpref.RespondentsPage{}, errors.New("forms respondents: no service token")
	}
	query := url.Values{}
	query.Set("actor", actor.String())
	if after != "" {
		query.Set("after", after)
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	address := strings.TrimRight(c.BaseURL, "/") + "/internal/v1/forms/" + formID.String() + "/respondents?" + query.Encode()
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return commpref.RespondentsPage{}, errors.New("forms respondents: request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: Timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		// The transport error quotes the address; keep only its cause.
		var addressed *url.Error
		if errors.As(err, &addressed) {
			err = addressed.Err
		}
		return commpref.RespondentsPage{}, fmt.Errorf("forms respondents: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return commpref.RespondentsPage{}, commpref.ErrFormUnknown
	case resp.StatusCode == http.StatusUnauthorized:
		// A token Forms refused: the next read asks for a new one.
		if invalidator, ok := c.Tokens.(interface{ Invalidate() }); ok {
			invalidator.Invalidate()
		}
		return commpref.RespondentsPage{}, fmt.Errorf("forms respondents: status %d", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return commpref.RespondentsPage{}, fmt.Errorf("forms respondents: status %d", resp.StatusCode)
	}
	var body page
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxBody))
	if err := decoder.Decode(&body); err != nil {
		return commpref.RespondentsPage{}, errors.New("forms respondents: answer is not the contract's JSON")
	}
	switch body.ActorRole {
	case commpref.FormRoleOwner, commpref.FormRoleEditor, commpref.FormRoleViewer, commpref.FormRoleNone:
	default:
		return commpref.RespondentsPage{}, errors.New("forms respondents: unknown actorRole")
	}
	out := commpref.RespondentsPage{ActorRole: body.ActorRole, ResponseCount: body.ResponseCount}
	if body.Next != nil {
		out.Next = *body.Next
	}
	for _, item := range body.Items {
		switch {
		case item.UserID != nil && item.Guest == nil:
			id, err := uuid.Parse(*item.UserID)
			if err != nil || id == uuid.Nil {
				return commpref.RespondentsPage{}, errors.New("forms respondents: an item's userId is not a subject")
			}
			out.Items = append(out.Items, commpref.Respondent{UserID: id})
		case item.Guest != nil && item.UserID == nil:
			out.Items = append(out.Items, commpref.Respondent{GuestName: item.Guest.Name, GuestEmail: item.Guest.Email})
		default:
			return commpref.RespondentsPage{}, errors.New("forms respondents: an item is neither an account nor a guest")
		}
	}
	return out, nil
}

var _ commpref.FormRespondentsSource = (*Client)(nil)

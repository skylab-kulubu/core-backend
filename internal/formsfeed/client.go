package formsfeed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// URLEnv is Forms' address on the internal network (the public one
	// answers /internal 404). Unset: ACCOUNT_ERASURE_FORMS_URL, the address
	// the Erasure command uses.
	URLEnv = "FORMS_INTERNAL_URL"
	// urlFallbackEnv is the Erasure command's Forms address.
	urlFallbackEnv = "ACCOUNT_ERASURE_FORMS_URL"
	// ReadRole is Forms' client role core's service account (client core)
	// needs for the internal reads (ADR-0071 karar 11). It is Forms': core
	// only names it.
	ReadRole = "skyforms:responses:read-service"
	// requestTimeout bounds one read, the token request included.
	requestTimeout = 15 * time.Second
	// maxBody bounds an answer Forms sends: a page of the list is at most
	// 200 small items.
	maxBody = 1 << 20
)

// URLFromEnv reads Forms' internal address and names the variable it came
// from; "" when neither is set. An error names the variable, never its value.
func URLFromEnv(getenv func(string) string) (base, from string, err error) {
	for _, name := range []string{URLEnv, urlFallbackEnv} {
		raw := strings.TrimSpace(getenv(name))
		if raw == "" {
			continue
		}
		parsed, err := url.Parse(raw)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
			parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
			return "", name, fmt.Errorf("%s must be an absolute http or https base URL without credentials, query or fragment", name)
		}
		return strings.TrimRight(raw, "/"), name, nil
	}
	return "", "", nil
}

// TokenSource is core's service account token (erasure.ClientCredentials
// without a scope): aud ∋ forms and ReadRole come from the realm.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	Invalidate()
}

// Errors of a read. Any other error is transient: no answer, a token that
// did not come, 401, 429 or 5xx.
var (
	// ErrNotFound is 404: the answer or the form is gone, or Forms has no
	// such route yet (core ships first).
	ErrNotFound = errors.New("formsfeed: not found in Forms (404)")
	// ErrForbidden is 403: a configuration error, core's service account
	// lacks ReadRole or aud ∋ forms. The realm's fix makes the next try go.
	ErrForbidden = errors.New("formsfeed: Forms refused core (403)")
)

// Client reads Forms' internal endpoints with core's service account.
type Client struct {
	BaseURL string
	Tokens  TokenSource
	// HTTP nil is a client with requestTimeout.
	HTTP *http.Client
}

// Response reads one answer, a guest's name and e-mail with it
// (ADR-0071 karar 11, claim check):
//
//	GET {base}/internal/v1/forms/{formId}/responses/{responseId}
//
// The answer is forms.response.created's data with guest {firstName,
// lastName, email} (null for a signed-in person's answer).
func (c *Client) Response(ctx context.Context, formID, responseID uuid.UUID) (Response, error) {
	body, err := c.get(ctx, "/internal/v1/forms/"+formID.String()+"/responses/"+responseID.String())
	if err != nil {
		return Response{}, err
	}
	r, err := DecodeResponse(body)
	if err != nil {
		return Response{}, err
	}
	if r.FormID != formID || r.ResponseID != responseID {
		return Response{}, invalid("Forms answered another answer")
	}
	return r, nil
}

// get sends one authorized GET and answers the body of a 200.
func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	token, err := c.Tokens.Token(ctx)
	if err != nil {
		return nil, errors.New("formsfeed: core's token did not come")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		// The transport's error is left out: it quotes the address.
		if ctx.Err() != nil {
			return nil, errors.New("formsfeed: Forms did not answer in time")
		}
		return nil, errors.New("formsfeed: Forms could not be reached")
	}
	defer resp.Body.Close()
	switch code := resp.StatusCode; {
	case code == http.StatusOK:
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		if err != nil {
			return nil, errors.New("formsfeed: Forms' answer broke off")
		}
		if len(body) > maxBody {
			return nil, invalid("Forms' answer is over %d bytes", maxBody)
		}
		return body, nil
	case code == http.StatusUnauthorized:
		c.Tokens.Invalidate()
		return nil, errors.New("formsfeed: Forms rejected core's token (401)")
	case code == http.StatusForbidden:
		// The fix (the role given, the audience mapped) reaches only a new
		// token.
		c.Tokens.Invalidate()
		return nil, ErrForbidden
	case code == http.StatusNotFound:
		return nil, ErrNotFound
	default:
		return nil, fmt.Errorf("formsfeed: Forms answered %d", code)
	}
}

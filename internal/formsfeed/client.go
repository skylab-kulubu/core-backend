package formsfeed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
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
//	GET {base}/internal/v1/forms/{formId}/responses/{responseId}?fields=…
//
// The answer is forms.response.created's data with guest {firstName,
// lastName, email} (null for a signed-in person's answer, and for a guest
// Forms has no name of). fields are the questions whose answers core asks
// for (an Event's form mapping, docs/form-mappings.md): Forms answers only
// those, in answers; without fields it sends none.
func (c *Client) Response(ctx context.Context, formID, responseID uuid.UUID, fields ...string) (Response, error) {
	path := "/internal/v1/forms/" + formID.String() + "/responses/" + responseID.String()
	if len(fields) > 0 {
		param, err := fieldsParam(fields)
		if err != nil {
			return Response{}, err
		}
		path += "?" + url.Values{"fields": {param}}.Encode()
	}
	body, err := c.get(ctx, path)
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

// PageLimit is how many answers core asks for a page of the list.
const PageLimit = 100

// ListQuery is a page of the list core asks for: the accepted answers
// changed since ChangedSince (nil: all of them), after the cursor After.
type ListQuery struct {
	ChangedSince *time.Time
	After        string
	// Fields are the questions whose answers core asks for with each item
	// (an Event's form mapping); none asks for no answer.
	Fields []string
}

// Page is a page of the list. Items are the answers that read; Skipped
// counts those that did not (another form's, an id or a version missing).
// NextCursor is empty on the last page, whose Watermark is Forms' time up
// to which the list is complete.
type Page struct {
	Items      []Response
	Skipped    int
	NextCursor string
	Watermark  *time.Time
}

type pageJSON struct {
	Items      []json.RawMessage `json:"items"`
	NextCursor *string           `json:"nextCursor"`
	Watermark  json.RawMessage   `json:"watermark"`
}

// List reads a page of a form's accepted answers (SPEC §10 eki):
//
//	GET {base}/internal/v1/forms/{formId}/responses?status=accepted&changedSince=…&after=…&limit=100
//
// answering {"items": [forms.response.created's data], "nextCursor",
// "watermark"}. 404 is ErrNotFound: the form is archived or deleted, or
// Forms has no such route yet.
func (c *Client) List(ctx context.Context, formID uuid.UUID, q ListQuery) (Page, error) {
	query := url.Values{}
	query.Set("status", StatusAccepted)
	if q.ChangedSince != nil {
		query.Set("changedSince", q.ChangedSince.UTC().Format(time.RFC3339))
	}
	if q.After != "" {
		query.Set("after", q.After)
	}
	query.Set("limit", strconv.Itoa(PageLimit))
	if len(q.Fields) > 0 {
		param, err := fieldsParam(q.Fields)
		if err != nil {
			return Page{}, err
		}
		query.Set("fields", param)
	}
	body, err := c.get(ctx, "/internal/v1/forms/"+formID.String()+"/responses?"+query.Encode())
	if err != nil {
		return Page{}, err
	}
	var in pageJSON
	if err := json.Unmarshal(body, &in); err != nil {
		return Page{}, invalid("Forms' page is not the expected JSON object")
	}
	page := Page{}
	if in.NextCursor != nil {
		page.NextCursor = *in.NextCursor
	}
	if len(page.NextCursor) > maxCursor {
		return Page{}, invalid("Forms' cursor is longer than %d bytes", maxCursor)
	}
	watermark, ignored := decodeTime(in.Watermark)
	if ignored {
		return Page{}, invalid("Forms' watermark is not an RFC 3339 time")
	}
	page.Watermark = watermark
	for _, raw := range in.Items {
		item, err := DecodeResponse(raw)
		if err != nil || item.FormID != formID {
			page.Skipped++
			continue
		}
		item.Guest = nil
		page.Items = append(page.Items, item)
	}
	return page, nil
}

// maxCursor bounds a cursor core keeps between pages (form_pulls.cursor).
const maxCursor = 2048

// Bounds of fields, Forms' (InternalEndpoints: at most 20 question ids, each
// at most 100 characters).
const (
	MaxFields      = 20
	MaxFieldLength = 100
)

// fieldsParam is the fields query value: the question ids, each once,
// sorted, joined by commas. An id that is empty, too long or carries a
// comma, or more than MaxFields, is core's own error (a mapping is checked
// when it is saved): ErrInvalid, before anything is sent.
func fieldsParam(fields []string) (string, error) {
	seen := map[string]bool{}
	var out []string
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" || len(f) > MaxFieldLength || strings.ContainsAny(f, ",") {
			return "", invalid("a question id of fields is empty, longer than %d or carries a comma", MaxFieldLength)
		}
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	if len(out) > MaxFields {
		return "", invalid("fields names more than %d questions", MaxFields)
	}
	sort.Strings(out)
	return strings.Join(out, ","), nil
}

// Question is one item of a form, as Forms' form read names it: its id
// (what a mapping keeps), its type, its text, the input type of a text
// question (email, name, …) and the identity field it fills (firstName,
// lastName, email), when it is one.
type Question struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Question  string `json:"question"`
	InputType string `json:"inputType,omitempty"`
	Identity  string `json:"identity,omitempty"`
}

// Form is a form's title and questions (Forms' form read).
type Form struct {
	FormID    uuid.UUID  `json:"formId"`
	Title     string     `json:"title"`
	Questions []Question `json:"questions"`
}

type formJSON struct {
	FormID    uuid.UUID `json:"formId"`
	Title     string    `json:"title"`
	Questions []struct {
		ID        string  `json:"id"`
		Type      string  `json:"type"`
		Question  string  `json:"question"`
		InputType *string `json:"inputType"`
		Identity  *string `json:"identity"`
	} `json:"questions"`
}

// Form reads a form's questions (forms-backend #43):
//
//	GET {base}/internal/v1/forms/{formId}
//
// answering {formId, title, questions: [{id, type, question, inputType,
// identity}]}. 404 is ErrNotFound: the form is deleted, or Forms has no such
// route yet. An item without an id is left out: nothing can map to it.
func (c *Client) Form(ctx context.Context, formID uuid.UUID) (Form, error) {
	body, err := c.get(ctx, "/internal/v1/forms/"+formID.String())
	if err != nil {
		return Form{}, err
	}
	var in formJSON
	if err := json.Unmarshal(body, &in); err != nil {
		return Form{}, invalid("Forms' form is not the expected JSON object")
	}
	if in.FormID != formID {
		return Form{}, invalid("Forms answered another form")
	}
	out := Form{FormID: in.FormID, Title: in.Title, Questions: []Question{}}
	for _, q := range in.Questions {
		if strings.TrimSpace(q.ID) == "" || len(q.ID) > MaxFieldLength {
			continue
		}
		item := Question{ID: q.ID, Type: q.Type, Question: q.Question}
		if q.InputType != nil {
			item.InputType = *q.InputType
		}
		if q.Identity != nil {
			item.Identity = *q.Identity
		}
		out.Questions = append(out.Questions, item)
	}
	return out, nil
}

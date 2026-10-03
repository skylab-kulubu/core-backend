package githubactivity

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	// DefaultAPIURL is GitHub's API.
	DefaultAPIURL = "https://api.github.com"
	// DefaultRequestTimeout bounds one request to GitHub.
	DefaultRequestTimeout = 15 * time.Second
	// tokenRenewBefore is how long before its expiry an installation token
	// is replaced. GitHub's last an hour.
	tokenRenewBefore = 5 * time.Minute
	// lowRemaining is the request budget left at which core stops asking
	// GitHub until the budget's reset: whatever else uses the installation
	// keeps the rest.
	lowRemaining = 20
	// maxBody bounds a GitHub answer core reads.
	maxBody = 8 << 20
)

// ErrRateLimited is GitHub's request budget spent (or nearly): core asks
// nothing until it resets.
var ErrRateLimited = errors.New("githubactivity: GitHub rate limit reached")

// tokenPermissions are the only permissions an installation token is asked
// for, whatever the app holds: read-only metadata, contents and pull requests.
var tokenPermissions = map[string]string{
	"metadata":      "read",
	"contents":      "read",
	"pull_requests": "read",
}

// client speaks to GitHub as the app's installation on one organisation.
type client struct {
	base  string
	http  *http.Client
	appID int64
	key   *rsa.PrivateKey
	org   string
	now   func() time.Time

	// tokenMu guards the installation and its token; it is held while a
	// token is fetched, so workers wait for one fetch instead of each making
	// their own.
	tokenMu      sync.Mutex
	installation int64
	// discover is true while the installation is found from the
	// organisation: a reinstalled app gets a new installation, found again.
	discover     bool
	token        string
	tokenExpires time.Time

	// limitMu guards limitedUntil, which every worker reads before a request.
	limitMu      sync.Mutex
	limitedUntil time.Time
}

func newClient(config Config, base string, httpClient *http.Client, now func() time.Time) *client {
	if base == "" {
		base = DefaultAPIURL
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultRequestTimeout}
	}
	if now == nil {
		now = time.Now
	}
	return &client{
		base: strings.TrimRight(base, "/"), http: httpClient, appID: config.AppID, key: config.PrivateKey,
		org: config.Org, now: now, installation: config.InstallationID, discover: config.InstallationID == 0,
	}
}

// appJWT is the app's own token: RS256, issued a minute in the past against
// clock drift, valid nine minutes (GitHub allows ten).
func (c *client) appJWT() (string, error) {
	now := c.now()
	claims := jwt.RegisteredClaims{
		Issuer:    strconv.FormatInt(c.appID, 10),
		IssuedAt:  jwt.NewNumericDate(now.Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(c.key)
	if err != nil {
		return "", errors.New("githubactivity: cannot sign the app token")
	}
	return signed, nil
}

// installationToken is the installation access token, kept until shortly
// before it expires.
func (c *client) installationToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.token != "" && c.now().Before(c.tokenExpires.Add(-tokenRenewBefore)) {
		return c.token, nil
	}
	appToken, err := c.appJWT()
	if err != nil {
		return "", err
	}
	if c.installation == 0 {
		var found struct {
			ID int64 `json:"id"`
		}
		if err := c.call(ctx, http.MethodGet, c.base+"/orgs/"+url.PathEscape(c.org)+"/installation", appToken, nil, &found, nil); err != nil {
			return "", fmt.Errorf("githubactivity: finding the app's installation on %s: %w", c.org, err)
		}
		if found.ID <= 0 {
			return "", fmt.Errorf("githubactivity: the app is not installed on %s", c.org)
		}
		c.installation = found.ID
	}
	body, _ := json.Marshal(map[string]any{"permissions": tokenPermissions})
	var issued struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := c.base + "/app/installations/" + strconv.FormatInt(c.installation, 10) + "/access_tokens"
	if err := c.call(ctx, http.MethodPost, path, appToken, body, &issued, nil); err != nil {
		var status *statusError
		if c.discover && errors.As(err, &status) && status.Status == http.StatusNotFound {
			c.installation = 0
		}
		return "", fmt.Errorf("githubactivity: installation token: %w", err)
	}
	if issued.Token == "" || issued.ExpiresAt.IsZero() {
		return "", errors.New("githubactivity: installation token: GitHub answered without a token")
	}
	c.token, c.tokenExpires = issued.Token, issued.ExpiresAt
	return c.token, nil
}

// forgetToken drops the installation token after GitHub refused it.
func (c *client) forgetToken() {
	c.tokenMu.Lock()
	c.token = ""
	c.tokenMu.Unlock()
}

// get reads one page of a REST path (or a next-page address GitHub gave) as
// the installation into out, and returns the next page's address, if any.
func (c *client) get(ctx context.Context, address string, out any) (next string, err error) {
	token, err := c.installationToken(ctx)
	if err != nil {
		return "", err
	}
	var header http.Header
	err = c.call(ctx, http.MethodGet, address, token, nil, out, &header)
	if errors.Is(err, errUnauthorized) {
		c.forgetToken()
	}
	if err != nil {
		return "", err
	}
	return c.nextPage(header)
}

// graphql runs a GraphQL query as the installation into out (its data).
func (c *client) graphql(ctx context.Context, query string, variables map[string]any, out any) error {
	token, err := c.installationToken(ctx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	var answer struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	err = c.call(ctx, http.MethodPost, c.base+"/graphql", token, body, &answer, nil)
	if errors.Is(err, errUnauthorized) {
		c.forgetToken()
	}
	if err != nil {
		return err
	}
	if len(answer.Errors) > 0 {
		if answer.Errors[0].Type == "RATE_LIMITED" {
			return ErrRateLimited
		}
		return fmt.Errorf("githubactivity: GraphQL: %s", answer.Errors[0].Message)
	}
	if err := json.Unmarshal(answer.Data, out); err != nil {
		return fmt.Errorf("githubactivity: GraphQL answer: %w", err)
	}
	return nil
}

var errUnauthorized = errors.New("githubactivity: GitHub refused the token")

// statusError is a GitHub answer other than 2xx. It names the path, never a
// token or a query string.
type statusError struct {
	Path   string
	Status int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("githubactivity: GitHub answered %d for %s", e.Status, e.Path)
}

// call makes one request. While GitHub's budget is spent (limitedUntil) it
// asks nothing and answers ErrRateLimited.
func (c *client) call(ctx context.Context, method, address, token string, body []byte, out any, header *http.Header) error {
	c.limitMu.Lock()
	limited := c.now().Before(c.limitedUntil)
	c.limitMu.Unlock()
	if limited {
		return ErrRateLimited
	}
	target, err := url.Parse(address)
	if err != nil || !c.ours(target) {
		// A next-page address elsewhere would carry the token there.
		return errors.New("githubactivity: refusing an address outside the GitHub API")
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, address, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "skylab-core-backend")
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("githubactivity: %s %s: %w", method, target.Path, errors.Unwrap(err))
	}
	defer response.Body.Close()
	c.noteRateLimit(response)
	if header != nil {
		*header = response.Header
	}
	switch {
	case response.StatusCode == http.StatusUnauthorized:
		return errUnauthorized
	case response.StatusCode == http.StatusTooManyRequests,
		response.StatusCode == http.StatusForbidden && (response.Header.Get("Retry-After") != "" || response.Header.Get("X-RateLimit-Remaining") == "0"):
		c.limitUntil(response)
		return ErrRateLimited
	case response.StatusCode < 200 || response.StatusCode > 299:
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxBody))
		return &statusError{Path: target.Path, Status: response.StatusCode}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxBody)).Decode(out); err != nil {
		return fmt.Errorf("githubactivity: reading %s: %w", target.Path, err)
	}
	return nil
}

func (c *client) ours(target *url.URL) bool {
	base, err := url.Parse(c.base)
	if err != nil {
		return false
	}
	return target.Scheme == base.Scheme && target.Host == base.Host &&
		strings.HasPrefix(target.Path, strings.TrimRight(base.Path, "/")+"/")
}

// noteRateLimit stops asking GitHub once the budget is nearly spent, until
// its reset.
func (c *client) noteRateLimit(response *http.Response) {
	remaining, err := strconv.Atoi(response.Header.Get("X-RateLimit-Remaining"))
	if err != nil || remaining > lowRemaining {
		return
	}
	c.limitUntil(response)
}

// limitUntil holds requests until GitHub's Retry-After or budget reset, and
// at least a minute when it names neither.
func (c *client) limitUntil(response *http.Response) {
	c.limitMu.Lock()
	defer c.limitMu.Unlock()
	now := c.now()
	until := now.Add(time.Minute)
	if seconds, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && seconds > 0 {
		until = now.Add(time.Duration(seconds) * time.Second)
	} else if reset, err := strconv.ParseInt(response.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && time.Unix(reset, 0).After(now) {
		until = time.Unix(reset, 0)
	}
	if until.After(c.limitedUntil) {
		c.limitedUntil = until
	}
}

// nextPage is the rel="next" address of a Link header.
func (c *client) nextPage(header http.Header) (string, error) {
	for _, link := range strings.Split(header.Get("Link"), ",") {
		parts := strings.Split(link, ";")
		if len(parts) < 2 {
			continue
		}
		isNext := false
		for _, p := range parts[1:] {
			if strings.TrimSpace(p) == `rel="next"` {
				isNext = true
			}
		}
		if !isNext {
			continue
		}
		address := strings.Trim(strings.TrimSpace(parts[0]), "<>")
		target, err := url.Parse(address)
		if err != nil || !c.ours(target) {
			return "", errors.New("githubactivity: refusing a next page outside the GitHub API")
		}
		return address, nil
	}
	return "", nil
}

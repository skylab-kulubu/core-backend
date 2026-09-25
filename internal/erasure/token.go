package erasure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// tokenRefreshMargin is how long before its expiry a cached token is dropped,
// so a token is never sent close to the end of its life.
const tokenRefreshMargin = 30 * time.Second

// ClientCredentials fetches the `core-erasure` client's token for one
// service's erase scope and keeps it until 30 seconds before it expires. It
// keeps nothing beyond that: a token whose lifetime is 30 seconds or less is
// not cached, a failure caches nothing, and Invalidate forgets the token when a
// service rejects it. The secret is the one configured at startup; a nightly
// rotation (ADR-0050) redeploys core, which re-reads it.
type ClientCredentials struct {
	TokenURL     string
	ClientID     string
	ClientSecret string
	Scope        string
	HTTP         *http.Client
	Now          func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

// Token returns a cached token or asks the token endpoint for a new one.
func (c *ClientCredentials) Token(ctx context.Context) (string, error) {
	now := c.now()
	c.mu.Lock()
	if c.token != "" && now.Before(c.expires) {
		token := c.token
		c.mu.Unlock()
		return token, nil
	}
	c.token, c.expires = "", time.Time{}
	c.mu.Unlock()

	token, lifetime, err := c.fetch(ctx)
	if err != nil {
		return "", err
	}
	if keep := lifetime - tokenRefreshMargin; keep > 0 {
		c.mu.Lock()
		c.token, c.expires = token, now.Add(keep)
		c.mu.Unlock()
	}
	return token, nil
}

// Invalidate forgets the cached token.
func (c *ClientCredentials) Invalidate() {
	c.mu.Lock()
	c.token, c.expires = "", time.Time{}
	c.mu.Unlock()
}

func (c *ClientCredentials) fetch(ctx context.Context) (string, time.Duration, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", c.ClientID)
	form.Set("client_secret", c.ClientSecret)
	form.Set("scope", "openid "+c.Scope)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, errors.New("token request could not be built")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: CommandTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		// The transport error names the token URL only, but it is dropped
		// anyway: a fixed message is all a log line needs here.
		return "", 0, fmt.Errorf("token endpoint unreachable (%s)", transportKind(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
		return "", 0, fmt.Errorf("token endpoint status %d", resp.StatusCode)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(&body); err != nil || body.AccessToken == "" {
		return "", 0, errors.New("token endpoint answered without an access token")
	}
	return body.AccessToken, time.Duration(body.ExpiresIn) * time.Second, nil
}

func (c *ClientCredentials) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

package authn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrIntrospectionUnavailable reports that the realm gave no usable answer
// about a token: it could not be reached, answered with an error status or a
// body that is not an RFC 7662 response, or refused Core's own client. It is
// never a verdict on the token, so callers must not treat it as a refusal.
var ErrIntrospectionUnavailable = errors.New("authn: token introspection unavailable")

// DefaultIntrospectionTimeout bounds one introspection call. The caller is a
// person waiting on a confirmation button, so a slow realm is reported as
// unavailable quickly instead of holding the request open.
const DefaultIntrospectionTimeout = 3 * time.Second

// maxIntrospectionResponse caps how much of the realm's answer is read. A
// Keycloak introspection response is a few hundred bytes.
const maxIntrospectionResponse = 64 << 10

// Introspector asks the realm what it knows about a token (RFC 7662). A nil
// error means the realm answered; whether the token is usable is in the
// returned claims (`active` and the rest). Any error means there was no
// answer, and it must never carry the token.
type Introspector interface {
	Introspect(ctx context.Context, token string) (map[string]any, error)
}

// Introspection calls `{issuer}/protocol/openid-connect/token/introspect` as
// Core's own confidential client, with the same client_secret_post
// credentials Core already uses for its client-credentials token. Keycloak
// answers `active:true` only for a token whose audience names the calling
// client, so this only works for tokens minted for Core.
type Introspection struct {
	URL          string
	ClientID     string
	ClientSecret string
	HTTP         *http.Client
	// Timeout bounds the whole call; zero means DefaultIntrospectionTimeout.
	Timeout time.Duration
}

func (i Introspection) Introspect(ctx context.Context, token string) (map[string]any, error) {
	timeout := i.Timeout
	if timeout <= 0 {
		timeout = DefaultIntrospectionTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	form := url.Values{}
	form.Set("token", token)
	form.Set("client_id", i.ClientID)
	form.Set("client_secret", i.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, i.URL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("%w: request could not be built", ErrIntrospectionUnavailable)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	client := i.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		// The error names the endpoint and the transport failure only: the
		// token and the client secret travel in the request body.
		return nil, fmt.Errorf("%w: %v", ErrIntrospectionUnavailable, err)
	}
	defer resp.Body.Close()
	// Keycloak answers 200 with `active:false` for a token it refuses. Any
	// other status is the realm, or Core's own client, failing - including a
	// 401 for rotated client credentials, which the person cannot fix by
	// re-authenticating.
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: introspection status %d", ErrIntrospectionUnavailable, resp.StatusCode)
	}
	var claims map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxIntrospectionResponse)).Decode(&claims); err != nil || claims == nil {
		return nil, fmt.Errorf("%w: introspection response is not a JSON object", ErrIntrospectionUnavailable)
	}
	return claims, nil
}

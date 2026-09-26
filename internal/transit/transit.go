// Package transit is core's client of the OpenBao Transit secrets engine for
// private Media (media redesign ticket 06, ADR-0052): it wraps and unwraps a
// Media's data key with the Transit key, so the key that protects every data
// key never leaves OpenBao.
//
// Core signs in with AppRole (role_id and secret_id at auth/approle/login)
// and keeps the token it gets: it renews the token in the background once
// half its lease has passed, logs in again when the token nears the end of
// its lease or its maximum lifetime, and logs in again once when OpenBao
// refuses the token. A token with lease left is used until a new one
// arrives. Callers that need a new token share one login, each waiting only
// as long as its own context allows, and a failed login is remembered for a
// few seconds. The core-media policy allows only encrypt, decrypt and rewrap
// with the key, so core makes each data key itself and has Transit encrypt
// it.
package transit

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	// ErrUnavailable is an OpenBao that cannot answer now: unreachable,
	// timing out, sealed, or failing (5xx, 429). Retrying later may work.
	ErrUnavailable = errors.New("transit: OpenBao is unavailable")
	// ErrDenied is an OpenBao that refuses core: the AppRole credentials are
	// wrong, or the token lacks the policy even after a fresh login.
	ErrDenied = errors.New("transit: OpenBao denied core's identity")
	// ErrRejected is a request OpenBao refuses as invalid, such as a
	// ciphertext of a key version it does not have.
	ErrRejected = errors.New("transit: OpenBao rejected the request")
	// ErrMisconfigured is an OpenBao without the mount or the key core is
	// configured with.
	ErrMisconfigured = errors.New("transit: OpenBao has no such Transit mount or key")
)

// DefaultTimeout bounds each request to OpenBao.
const DefaultTimeout = 5 * time.Second

// expirySkew is how long before its lease ends a token is no longer used.
const expirySkew = 30 * time.Second

// loginFailureHold is how long a failed login is answered from memory.
const loginFailureHold = 5 * time.Second

// Config is how core reaches the Transit key (docs/media-lifecycle.md).
type Config struct {
	// Addr is OpenBao's address: MEDIA_OPENBAO_ADDR.
	Addr string
	// Mount is the Transit mount: MEDIA_TRANSIT_MOUNT, e.g. transit/sandbox.
	Mount string
	// Key is the Transit key: MEDIA_TRANSIT_KEY.
	Key string
	// RoleID and SecretID are core's AppRole credentials:
	// MEDIA_OPENBAO_ROLE_ID and MEDIA_OPENBAO_SECRET_ID.
	RoleID   string
	SecretID string
	// Timeout bounds each request to OpenBao; DefaultTimeout when zero.
	Timeout time.Duration
	// Now defaults to time.Now.
	Now func() time.Time
}

// Client wraps and unwraps data keys. It is safe for concurrent use.
type Client struct {
	addr, mount, key string
	roleID, secretID string
	timeout          time.Duration
	http             *http.Client
	now              func() time.Time

	mu        sync.Mutex
	token     string
	expires   time.Time
	renewAt   time.Time
	renewable bool
	// refreshing is the login or renewal in progress, shared by callers.
	refreshing *refresh
	// loginErr is the last failed login, answered until loginErrUntil.
	loginErr      error
	loginErrUntil time.Time
}

// refresh is one login or renewal; done closes when it has finished.
type refresh struct {
	done chan struct{}
}

// New returns a client. It does not reach OpenBao: the first call logs in,
// so an OpenBao that is down never stops core from starting.
func New(cfg Config) *Client {
	c := &Client{
		addr: strings.TrimRight(cfg.Addr, "/"), mount: strings.Trim(cfg.Mount, "/"), key: cfg.Key,
		roleID: cfg.RoleID, secretID: cfg.SecretID, timeout: cfg.Timeout, now: cfg.Now,
	}
	if c.timeout <= 0 {
		c.timeout = DefaultTimeout
	}
	// A redirect is never followed: the token would go wherever it points.
	c.http = &http.Client{Timeout: c.timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	if c.now == nil {
		c.now = time.Now
	}
	return c
}

// WrapKey has Transit encrypt a data key. It returns the ciphertext and the
// key version that encrypted it.
func (c *Client) WrapKey(ctx context.Context, dataKey []byte) (string, int, error) {
	var out struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
		} `json:"data"`
	}
	if err := c.call(ctx, opEncrypt, map[string]string{"plaintext": base64.StdEncoding.EncodeToString(dataKey)}, &out); err != nil {
		return "", 0, err
	}
	version, err := KeyVersion(out.Data.Ciphertext)
	if err != nil {
		return "", 0, fmt.Errorf("%w: encrypt answered %v", ErrUnavailable, err)
	}
	return out.Data.Ciphertext, version, nil
}

// UnwrapKey has Transit decrypt a wrapped data key, under whichever key
// version wrapped it.
func (c *Client) UnwrapKey(ctx context.Context, ciphertext string) ([]byte, error) {
	var out struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	if err := c.call(ctx, opDecrypt, map[string]string{"ciphertext": ciphertext}, &out); err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(out.Data.Plaintext)
	if err != nil {
		return nil, fmt.Errorf("%w: decrypt answered a plaintext that is not base64", ErrUnavailable)
	}
	return key, nil
}

// KeyVersion is the key version a Transit ciphertext names in its
// "vault:v<version>:" prefix.
func KeyVersion(ciphertext string) (int, error) {
	rest, ok := strings.CutPrefix(ciphertext, "vault:v")
	if !ok {
		return 0, errors.New("transit: ciphertext has no vault:v prefix")
	}
	number, body, ok := strings.Cut(rest, ":")
	if !ok || body == "" || number == "" || strings.Trim(number, "0123456789") != "" {
		return 0, errors.New("transit: ciphertext has no key version")
	}
	version, err := strconv.Atoi(number)
	if err != nil || version < 1 {
		return 0, errors.New("transit: ciphertext has no key version")
	}
	return version, nil
}

// The requests the client makes; they decide what a 4xx answer means.
const (
	opEncrypt = "encrypt"
	opDecrypt = "decrypt"
	opLogin   = "login"
	opRenew   = "renew"
)

// call runs a Transit operation with the key. A token OpenBao refuses is
// dropped and the operation runs once more with a fresh login.
func (c *Client) call(ctx context.Context, operation string, body, out any) error {
	path := "/v1/" + c.mount + "/" + operation + "/" + c.key
	for attempt := 0; ; attempt++ {
		token, err := c.currentToken(ctx)
		if err != nil {
			return err
		}
		err = c.post(ctx, operation, path, token, body, out)
		if errors.Is(err, ErrDenied) && attempt == 0 {
			c.forget(token)
			continue
		}
		return err
	}
}

// forget drops a token OpenBao refused.
func (c *Client) forget(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == token {
		c.expires = time.Time{}
	}
}

// currentToken returns a token with lease left. Past half its lease it
// starts a renewal in the background and returns the token meanwhile.
// Without one it waits, for as long as ctx allows, for the login in
// progress (starting one if none is), unless a login failed a moment ago.
func (c *Client) currentToken(ctx context.Context) (string, error) {
	for {
		c.mu.Lock()
		now := c.now()
		if c.token != "" && now.Before(c.expires.Add(-expirySkew)) {
			token := c.token
			if c.renewable && !now.Before(c.renewAt) && c.refreshing == nil {
				c.startRefresh(true)
			}
			c.mu.Unlock()
			return token, nil
		}
		if c.loginErr != nil && now.Before(c.loginErrUntil) {
			err := c.loginErr
			c.mu.Unlock()
			return "", err
		}
		if c.refreshing == nil {
			c.startRefresh(false)
		}
		done := c.refreshing.done
		c.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return "", fmt.Errorf("%w: waiting for an OpenBao token: %w", ErrUnavailable, ctx.Err())
		}
		c.mu.Lock()
		failed := c.loginErr != nil && c.now().Before(c.loginErrUntil)
		err := c.loginErr
		c.mu.Unlock()
		if failed {
			return "", err
		}
	}
}

// startRefresh starts the one login or renewal all callers share. It runs
// on its own deadline, so a caller that gives up does not end it. c.mu is
// held.
func (c *Client) startRefresh(renew bool) {
	r := &refresh{done: make(chan struct{})}
	c.refreshing = r
	token := c.token
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
		defer cancel()
		var auth authResponse
		var err error
		if renew {
			err = c.post(ctx, opRenew, "/v1/auth/token/renew-self", token, map[string]string{}, &auth)
			if err == nil && auth.Auth.ClientToken != "" && auth.Auth.ClientToken != token {
				err = fmt.Errorf("%w: renewal answered another token", ErrUnavailable)
			}
			auth.Auth.ClientToken = token
		}
		if !renew || err != nil {
			auth, err = c.login(ctx)
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if err == nil {
			c.keep(auth, c.now())
			c.loginErr = nil
		} else {
			// The token in hand, if it still has lease, stays in use.
			c.loginErr = err
			c.loginErrUntil = c.now().Add(loginFailureHold)
		}
		c.refreshing = nil
		close(r.done)
	}()
}

type authResponse struct {
	Auth struct {
		ClientToken   string `json:"client_token"`
		LeaseDuration int64  `json:"lease_duration"`
		Renewable     bool   `json:"renewable"`
	} `json:"auth"`
}

func (c *Client) login(ctx context.Context) (authResponse, error) {
	var out authResponse
	err := c.post(ctx, opLogin, "/v1/auth/approle/login", "", map[string]string{"role_id": c.roleID, "secret_id": c.secretID}, &out)
	if errors.Is(err, ErrRejected) {
		// OpenBao answers wrong AppRole credentials with 400.
		return authResponse{}, fmt.Errorf("%w: AppRole login refused", ErrDenied)
	}
	if err != nil {
		return authResponse{}, err
	}
	if out.Auth.ClientToken == "" {
		return authResponse{}, fmt.Errorf("%w: AppRole login answered no token", ErrUnavailable)
	}
	return out, nil
}

// keep takes a token and its lease. c.mu is held.
func (c *Client) keep(out authResponse, now time.Time) {
	lease := time.Duration(out.Auth.LeaseDuration) * time.Second
	c.token = out.Auth.ClientToken
	c.expires = now.Add(lease)
	c.renewAt = now.Add(lease / 2)
	c.renewable = out.Auth.Renewable
}

func (c *Client) post(ctx context.Context, operation, path, token string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.addr+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrUnavailable, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrUnavailable, path, err)
	}
	status := resp.StatusCode
	switch {
	case status >= 200 && status < 300:
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%w: %s answered %d with a body that is not JSON", ErrUnavailable, path, status)
		}
		return nil
	case status == http.StatusForbidden:
		return fmt.Errorf("%w: %s answered %d: %s", ErrDenied, path, status, openBaoErrors(raw))
	case status == http.StatusTooManyRequests || status >= 500:
		return fmt.Errorf("%w: %s answered %d: %s", ErrUnavailable, path, status, openBaoErrors(raw))
	case status >= 300 && status < 400:
		return fmt.Errorf("%w: %s answered a redirect (%d), which core does not follow", ErrUnavailable, path, status)
	}
	message := openBaoErrors(raw)
	// A missing mount is 404, a missing key "encryption key not found", and
	// nothing core sends to encrypt can be invalid: those are OpenBao's
	// configuration. Anything else refuses the ciphertext.
	if status == http.StatusNotFound || strings.Contains(message, "not found") || operation == opEncrypt {
		return fmt.Errorf("%w: %s answered %d: %s", ErrMisconfigured, path, status, message)
	}
	return fmt.Errorf("%w: %s answered %d: %s", ErrRejected, path, status, message)
}

// openBaoErrors is the "errors" list of an OpenBao error answer. It names
// what went wrong, never a token or a secret.
func openBaoErrors(raw []byte) string {
	var body struct {
		Errors []string `json:"errors"`
	}
	if json.Unmarshal(raw, &body) != nil || len(body.Errors) == 0 {
		return "no error message"
	}
	message := strings.Join(body.Errors, "; ")
	if len(message) > 200 {
		message = message[:200]
	}
	return message
}

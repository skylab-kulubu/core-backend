// Package googlewallet is core's small client of the Google Wallet API
// (https://developers.google.com/wallet/generic): the issuer's service
// account writes Generic pass classes and objects over REST and signs the
// "Add to Google Wallet" links (https://pay.google.com/gp/v/save/<jwt>).
//
// Nothing here logs. Errors name the operation, the HTTP status and Google's
// error status, never a token, an assertion, a key or a pass's content.
package googlewallet

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	// Scope is the OAuth scope of the Wallet API's issuer calls.
	Scope = "https://www.googleapis.com/auth/wallet_object.issuer"
	// DefaultAPIBase is the Wallet API's REST root.
	DefaultAPIBase = "https://walletobjects.googleapis.com/walletobjects/v1"
	// DefaultTokenURL is Google's OAuth token endpoint, used when the key
	// names none.
	DefaultTokenURL = "https://oauth2.googleapis.com/token"
	// SaveURLPrefix starts every "Add to Google Wallet" link.
	SaveURLPrefix = "https://pay.google.com/gp/v/save/"
	// MaxSaveJWTLength is the length Google calls safe for the signed JWT
	// of a save link; longer links may be cut by browsers, so SaveURL
	// refuses them.
	MaxSaveJWTLength = 1800

	// DefaultTimeout bounds each request to Google.
	DefaultTimeout = 10 * time.Second
	// tokenLifetime is how long the assertion asks the token to live (the
	// most Google grants), and tokenRenewBefore how long before its expiry
	// a cached token is replaced.
	tokenLifetime    = time.Hour
	tokenRenewBefore = 5 * time.Minute
	maxResponseBytes = 1 << 20
)

var (
	// ErrNotFound is a class or object Google does not have.
	ErrNotFound = errors.New("googlewallet: not found")
	// ErrConflict is an insert of a class or object that already exists.
	ErrConflict = errors.New("googlewallet: already exists")
	// ErrInvalidAccount is a service account key that cannot be used. It
	// never carries the key.
	ErrInvalidAccount = errors.New("googlewallet: invalid service account key")
)

// APIError is a refusal or failure from Google.
type APIError struct {
	// Op is what core asked: token, insert class, patch class, insert
	// object, update object.
	Op string
	// Status is the HTTP status; 0 when no answer came.
	Status int
	// Reason is Google's error status (INVALID_ARGUMENT, invalid_grant, …),
	// never its message.
	Reason string
	err    error
}

func (e *APIError) Error() string {
	msg := "googlewallet: " + e.Op
	if e.Status != 0 {
		msg += fmt.Sprintf(": HTTP %d", e.Status)
	}
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	if e.err != nil {
		msg += ": " + e.err.Error()
	}
	return msg
}

func (e *APIError) Unwrap() error { return e.err }

func (e *APIError) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.Status == http.StatusNotFound
	case ErrConflict:
		return e.Status == http.StatusConflict
	}
	return false
}

// ServiceAccount is the parsed JSON key of the issuer's service account.
type ServiceAccount struct {
	ClientEmail  string
	PrivateKeyID string
	TokenURI     string
	key          *rsa.PrivateKey
}

type serviceAccountJSON struct {
	Type         string `json:"type"`
	ClientEmail  string `json:"client_email"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	TokenURI     string `json:"token_uri"`
}

// ParseServiceAccount reads the JSON key Google's console downloads for a
// service account, as it is or base64-encoded on one line (how an OpenBao
// reference carries it). Its token_uri must be https.
func ParseServiceAccount(raw string) (ServiceAccount, error) {
	return parseServiceAccount(raw, false)
}

func parseServiceAccount(raw string, allowHTTPToken bool) (ServiceAccount, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ServiceAccount{}, fmt.Errorf("%w: empty", ErrInvalidAccount)
	}
	body := []byte(raw)
	if !strings.HasPrefix(raw, "{") {
		decoded, err := decodeBase64(raw)
		if err != nil {
			return ServiceAccount{}, fmt.Errorf("%w: neither JSON nor base64", ErrInvalidAccount)
		}
		body = bytes.TrimSpace(decoded)
	}
	var parsed serviceAccountJSON
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ServiceAccount{}, fmt.Errorf("%w: not JSON", ErrInvalidAccount)
	}
	if parsed.Type != "service_account" {
		return ServiceAccount{}, fmt.Errorf("%w: type is not service_account", ErrInvalidAccount)
	}
	if strings.TrimSpace(parsed.ClientEmail) == "" {
		return ServiceAccount{}, fmt.Errorf("%w: client_email is missing", ErrInvalidAccount)
	}
	key, err := parseRSAKey(parsed.PrivateKey)
	if err != nil {
		return ServiceAccount{}, err
	}
	tokenURI := strings.TrimSpace(parsed.TokenURI)
	if tokenURI == "" {
		tokenURI = DefaultTokenURL
	}
	u, err := url.Parse(tokenURI)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(allowHTTPToken && u.Scheme == "http")) {
		return ServiceAccount{}, fmt.Errorf("%w: token_uri must be an https URL", ErrInvalidAccount)
	}
	return ServiceAccount{
		ClientEmail:  strings.TrimSpace(parsed.ClientEmail),
		PrivateKeyID: strings.TrimSpace(parsed.PrivateKeyID),
		TokenURI:     tokenURI,
		key:          key,
	}, nil
}

// decodeBase64 reads standard, padded base64 as `base64` prints it; line
// breaks and spaces are ignored.
func decodeBase64(raw string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(strings.Join(strings.Fields(raw), ""))
}

func parseRSAKey(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, fmt.Errorf("%w: private_key is not PEM", ErrInvalidAccount)
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if key, ok := parsed.(*rsa.PrivateKey); ok {
			return key, nil
		}
		return nil, fmt.Errorf("%w: private_key is not RSA", ErrInvalidAccount)
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("%w: private_key cannot be read", ErrInvalidAccount)
}

// Options are a Client's optional settings.
type Options struct {
	// APIBase replaces DefaultAPIBase (tests).
	APIBase string
	// HTTP is the client for every request. Nil uses one with
	// DefaultTimeout.
	HTTP *http.Client
	// Now is the clock of tokens and links. Nil is time.Now.
	Now func() time.Time
}

// Client speaks to the Wallet API as one issuer's service account.
type Client struct {
	account ServiceAccount
	apiBase string
	http    *http.Client
	now     func() time.Time

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

func NewClient(account ServiceAccount, opts Options) *Client {
	base := strings.TrimRight(strings.TrimSpace(opts.APIBase), "/")
	if base == "" {
		base = DefaultAPIBase
	}
	client := opts.HTTP
	if client == nil {
		client = &http.Client{Timeout: DefaultTimeout}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Client{account: account, apiBase: base, http: client, now: now}
}

// ObjectRef names an object in a save link: only its id and class, so
// nothing the object holds travels in the link.
type ObjectRef struct {
	ID      string `json:"id"`
	ClassID string `json:"classId"`
}

// SaveURL signs an "Add to Google Wallet" link for objects core has already
// inserted. origins are the web origins allowed to show Google's save
// button for it; an empty list is sent as [] (the claim is required).
func (c *Client) SaveURL(origins []string, objects []ObjectRef) (string, error) {
	if c == nil || c.account.key == nil {
		return "", ErrInvalidAccount
	}
	if origins == nil {
		origins = []string{}
	}
	if objects == nil {
		objects = []ObjectRef{}
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss":     c.account.ClientEmail,
		"aud":     "google",
		"typ":     "savetowallet",
		"iat":     c.now().Unix(),
		"origins": origins,
		"payload": map[string]any{"genericObjects": objects},
	})
	if c.account.PrivateKeyID != "" {
		token.Header["kid"] = c.account.PrivateKeyID
	}
	signed, err := token.SignedString(c.account.key)
	if err != nil {
		return "", errors.New("googlewallet: cannot sign the save link")
	}
	if len(signed) > MaxSaveJWTLength {
		// A browser may cut it: the person would get a broken link.
		return "", fmt.Errorf("googlewallet: the save link's JWT is %d characters, over %d; name fewer origins", len(signed), MaxSaveJWTLength)
	}
	return SaveURLPrefix + signed, nil
}

// EnsureGenericClass inserts the class, or patches it to these settings when
// it already exists.
func (c *Client) EnsureGenericClass(ctx context.Context, class GenericClass) error {
	err := c.send(ctx, "insert class", http.MethodPost, "/genericClass", class)
	if errors.Is(err, ErrConflict) {
		return c.send(ctx, "patch class", http.MethodPatch, "/genericClass/"+url.PathEscape(class.ID), class)
	}
	return err
}

// InsertGenericObject creates the object; ErrConflict when it exists.
func (c *Client) InsertGenericObject(ctx context.Context, object GenericObject) error {
	return c.send(ctx, "insert object", http.MethodPost, "/genericObject", object)
}

// UpdateGenericObject replaces the object with this one (PUT): fields left
// out are cleared. ErrNotFound when Google has no such object.
func (c *Client) UpdateGenericObject(ctx context.Context, object GenericObject) error {
	return c.send(ctx, "update object", http.MethodPut, "/genericObject/"+url.PathEscape(object.ID), object)
}

func (c *Client) send(ctx context.Context, op, method, path string, body any) error {
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return &APIError{Op: op, err: errors.New("cannot encode the request")}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase+path, bytes.NewReader(payload))
	if err != nil {
		return &APIError{Op: op, err: errors.New("cannot build the request")}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return &APIError{Op: op, err: transportError(err)}
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// A token Google no longer takes is dropped, so the next call
		// asks for a new one.
		c.mu.Lock()
		c.token = ""
		c.mu.Unlock()
	}
	return &APIError{Op: op, Status: resp.StatusCode, Reason: apiReason(answer)}
}

// accessToken returns a cached OAuth token or asks for a new one with a
// signed assertion (RFC 7523 JWT bearer grant). One request at a time.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.token != "" && now.Before(c.tokenExp.Add(-tokenRenewBefore)) {
		return c.token, nil
	}
	if c.account.key == nil {
		return "", ErrInvalidAccount
	}
	assertion := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss":   c.account.ClientEmail,
		"scope": Scope,
		"aud":   c.account.TokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(tokenLifetime).Unix(),
	})
	if c.account.PrivateKeyID != "" {
		assertion.Header["kid"] = c.account.PrivateKeyID
	}
	signed, err := assertion.SignedString(c.account.key)
	if err != nil {
		return "", &APIError{Op: "token", err: errors.New("cannot sign the assertion")}
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {signed},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.account.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", &APIError{Op: "token", err: errors.New("cannot build the request")}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", &APIError{Op: "token", err: transportError(err)}
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	var got struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
	}
	_ = json.Unmarshal(answer, &got)
	if resp.StatusCode != http.StatusOK || got.AccessToken == "" {
		return "", &APIError{Op: "token", Status: resp.StatusCode, Reason: safeWord(got.Error)}
	}
	lifetime := time.Duration(got.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = tokenLifetime
	}
	c.token, c.tokenExp = got.AccessToken, now.Add(lifetime)
	return c.token, nil
}

// transportError keeps what failed (a timeout, a refused connection) without
// the request's URL, which is all net/http would add.
func transportError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if urlErr.Timeout() {
			return errors.New("timeout")
		}
		return errors.New("request failed")
	}
	return errors.New("request failed")
}

var wordOnly = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// apiReason is Google's error status (INVALID_ARGUMENT, PERMISSION_DENIED,
// …). Its message is left out: it may quote what was sent, a name or the
// TOTP key among it.
func apiReason(body []byte) string {
	var parsed struct {
		Error struct {
			Status string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	return safeWord(parsed.Error.Status)
}

func safeWord(s string) string {
	s = strings.TrimSpace(s)
	if wordOnly.MatchString(s) {
		return s
	}
	return ""
}

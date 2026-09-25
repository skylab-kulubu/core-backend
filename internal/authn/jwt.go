package authn

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

var ErrInvalidToken = errors.New("authn: invalid token")

const ResourceAudience = "core"

// AccountAudience is the Keycloak Account REST audience the Account Center
// end-user token must carry. Keycloak writes `aud` as a bare string or as an
// array, and the Account Center client now also resolves Core's audience, so
// the claim is read as a set that must contain this value rather than as one
// exact string.
const AccountAudience = "account"

func ParseAccessToken(token string) (Identity, error) {
	ident, _, err := decodeAccessToken(token)
	return ident, err
}

func ParseAndVerify(token string, verify func(string) error, issuer, audience string) (Identity, error) {
	if verify == nil || strings.TrimSpace(issuer) == "" || strings.TrimSpace(audience) == "" {
		return Identity{}, ErrInvalidToken
	}
	if err := verify(token); err != nil {
		return Identity{}, ErrInvalidToken
	}
	ident, claims, err := decodeAccessToken(token)
	if err != nil {
		return Identity{}, err
	}
	iss, _ := claims["iss"].(string)
	if iss != issuer {
		return Identity{}, ErrInvalidToken
	}
	if !audienceIncludes(claims["aud"], audience) {
		return Identity{}, ErrInvalidToken
	}
	return ident, nil
}

// ParseSelfDeleteContext verifies the narrow two-token end-user contract used
// only by the Account Center self-deletion intake. The Account REST access
// token proves the caller is a user of the confidential Account Center client;
// the separately signature-verified ID token proves recent authentication.
//
// The access token's audience is a set that must contain AccountAudience: the
// Account Center client resolves further audiences (Core's own among them), so
// requiring one exact string would reject every reconciled token. The ID token
// audience stays exclusive - a re-authentication proof may name the Account
// Center client and nothing else.
func ParseSelfDeleteContext(accessToken, idToken string, verify func(string) error, issuer, clientID string, now time.Time, maxAuthenticationAge time.Duration) (Identity, error) {
	if verify == nil || strings.TrimSpace(issuer) == "" || strings.TrimSpace(clientID) == "" || maxAuthenticationAge <= 0 {
		return Identity{}, ErrInvalidToken
	}
	now = now.UTC()
	ident, _, err := verifySelfDeleteBearer(accessToken, verify, issuer, clientID, now)
	if err != nil {
		return Identity{}, err
	}

	if err := verify(idToken); err != nil {
		return Identity{}, ErrInvalidToken
	}
	idHeader, err := decodeJWTObject(strings.Split(strings.TrimSpace(idToken), "."), 0)
	if err != nil || idHeader["alg"] != "RS256" || idHeader["typ"] != "JWT" {
		return Identity{}, ErrInvalidToken
	}
	reauthenticated, idClaims, err := decodeAccessToken(idToken)
	if err != nil || reauthenticated.ID != ident.ID {
		return Identity{}, ErrInvalidToken
	}
	if !audienceIsOnly(idClaims["aud"], clientID) || idClaims["iss"] != issuer {
		return Identity{}, ErrInvalidToken
	}
	sid, ok := idClaims["sid"].(string)
	if !ok || strings.TrimSpace(sid) == "" {
		return Identity{}, ErrInvalidToken
	}
	authenticatedAt, ok := integerTimeClaim(idClaims["auth_time"])
	if !ok {
		return Identity{}, ErrInvalidToken
	}
	if authenticatedAt.After(now.Add(5*time.Second)) || authenticatedAt.Before(now.Add(-maxAuthenticationAge)) {
		return Identity{}, ErrInvalidToken
	}
	idExpiresAt, ok := integerTimeClaim(idClaims["exp"])
	if !ok || !idExpiresAt.After(now) {
		return Identity{}, ErrInvalidToken
	}
	return ident, nil
}

// SudoTokenType and SudoAudience describe the sky-account Sudo mode token:
// Keycloak's sky-account extension mints it after the person re-proves
// themselves inside Account Center, and it names Core in its audience next to
// SudoAudience so that Core may introspect it.
const (
	SudoTokenType = "sky-sudo"
	SudoAudience  = "sky-account"
)

// ParseSelfDeleteSudoContext verifies the self-deletion intake when Account
// Center proves recent authentication with a Sudo mode token instead of a
// fresh ID token. The bearer is checked exactly as in ParseSelfDeleteContext
// and must carry the Keycloak session id (`sid`) that sky-account itself binds
// every sudo token to.
//
// The sudo token is signed with the realm's internal HMAC key, which never
// leaves Keycloak, so it cannot be verified against the JWKS. Core asks the
// realm instead (RFC 7662) and requires an active token with `typ=sky-sudo`,
// the realm issuer, `azp` of the Account Center client, an audience set
// containing both `sky-account` and `core`, and the bearer's own subject and
// session. Freshness is the token's own five-minute lifetime (a future
// integer `exp`); the ID-token `auth_time` rule does not apply, because an
// in-product proof never moves the session's `auth_time`.
//
// An error wrapping ErrIntrospectionUnavailable means the realm could not be
// asked; every refusal is ErrInvalidToken.
func ParseSelfDeleteSudoContext(ctx context.Context, accessToken, sudoToken string, verify func(string) error, introspect Introspector, issuer, clientID string, now time.Time) (Identity, error) {
	if verify == nil || introspect == nil || strings.TrimSpace(issuer) == "" || strings.TrimSpace(clientID) == "" || sudoToken == "" {
		return Identity{}, ErrInvalidToken
	}
	now = now.UTC()
	// The bearer is verified locally first, so only a real Account Center
	// session can make Core call the realm.
	ident, sessionID, err := verifySelfDeleteSessionBearer(accessToken, verify, issuer, clientID, now)
	if err != nil {
		return Identity{}, err
	}

	proof, err := introspect.Introspect(ctx, sudoToken)
	if err != nil {
		if errors.Is(err, ErrIntrospectionUnavailable) {
			return Identity{}, err
		}
		// A foreign error is not propagated: it could carry the token.
		return Identity{}, ErrIntrospectionUnavailable
	}
	if active, _ := proof["active"].(bool); !active {
		return Identity{}, ErrInvalidToken
	}
	if proof["typ"] != SudoTokenType || proof["iss"] != issuer || proof["azp"] != clientID {
		return Identity{}, ErrInvalidToken
	}
	if !audienceIncludes(proof["aud"], SudoAudience) || !audienceIncludes(proof["aud"], ResourceAudience) {
		return Identity{}, ErrInvalidToken
	}
	if proof["sub"] != ident.ID.String() || proof["sid"] != sessionID {
		return Identity{}, ErrInvalidToken
	}
	expiresAt, ok := integerTimeClaim(proof["exp"])
	if !ok || !expiresAt.After(now) {
		return Identity{}, ErrInvalidToken
	}
	return ident, nil
}

// ParseSelfDeleteBearer verifies the Account Center bearer of the
// self-deletion intake on its own, with exactly the local rules of the sudo
// path: RS256 against the realm JWKS, `typ=JWT`, the realm issuer, `azp` of
// the Account Center client, an audience set containing `account`, exactly
// `scope=openid`, a canonical UUID subject, a future integer `exp` and a
// non-empty `sid`. It never asks the realm anything.
//
// It proves who the caller is, not that they recently re-authenticated. The
// intake uses it only to answer a replay of an idempotency key Core already
// accepted for that same subject: the deletion saga closes the Keycloak
// session the sudo proof is bound to, after which introspection calls the
// proof inactive although the request it authorised is under way.
func ParseSelfDeleteBearer(accessToken string, verify func(string) error, issuer, clientID string, now time.Time) (Identity, error) {
	if verify == nil || strings.TrimSpace(issuer) == "" || strings.TrimSpace(clientID) == "" {
		return Identity{}, ErrInvalidToken
	}
	ident, _, err := verifySelfDeleteSessionBearer(accessToken, verify, issuer, clientID, now.UTC())
	return ident, err
}

// verifySelfDeleteSessionBearer is verifySelfDeleteBearer plus the Keycloak
// session id (`sid`) the sudo path binds every proof to, which it returns.
func verifySelfDeleteSessionBearer(accessToken string, verify func(string) error, issuer, clientID string, now time.Time) (Identity, string, error) {
	ident, claims, err := verifySelfDeleteBearer(accessToken, verify, issuer, clientID, now)
	if err != nil {
		return Identity{}, "", err
	}
	sessionID, ok := claims["sid"].(string)
	if !ok || strings.TrimSpace(sessionID) == "" {
		return Identity{}, "", ErrInvalidToken
	}
	return ident, sessionID, nil
}

// verifySelfDeleteBearer applies the Account Center bearer rules shared by
// both self-deletion proofs and returns the identity and the verified claims.
func verifySelfDeleteBearer(accessToken string, verify func(string) error, issuer, clientID string, now time.Time) (Identity, map[string]any, error) {
	if err := verify(accessToken); err != nil {
		return Identity{}, nil, ErrInvalidToken
	}
	header, err := decodeJWTObject(strings.Split(strings.TrimSpace(accessToken), "."), 0)
	if err != nil || header["alg"] != "RS256" || header["typ"] != "JWT" {
		return Identity{}, nil, ErrInvalidToken
	}
	ident, claims, err := decodeAccessToken(accessToken)
	if err != nil {
		return Identity{}, nil, err
	}
	if !audienceIncludes(claims["aud"], AccountAudience) || claims["iss"] != issuer || claims["azp"] != clientID || claims["scope"] != "openid" {
		return Identity{}, nil, ErrInvalidToken
	}
	expiresAt, ok := integerTimeClaim(claims["exp"])
	if !ok || !expiresAt.After(now) {
		return Identity{}, nil, ErrInvalidToken
	}
	return ident, claims, nil
}

func decodeAccessToken(token string) (Identity, map[string]any, error) {
	token = strings.TrimSpace(token)
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return Identity{}, nil, ErrInvalidToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		padded := parts[1]
		switch len(padded) % 4 {
		case 2:
			padded += "=="
		case 3:
			padded += "="
		}
		payload, err = base64.URLEncoding.DecodeString(padded)
		if err != nil {
			return Identity{}, nil, ErrInvalidToken
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Identity{}, nil, ErrInvalidToken
	}
	sub, _ := claims["sub"].(string)
	id, err := uuid.Parse(sub)
	if err != nil || id.String() != sub {
		return Identity{}, nil, ErrInvalidToken
	}
	email, _ := claims["email"].(string)
	given, _ := claims["given_name"].(string)
	family, _ := claims["family_name"].(string)
	school, _ := claims["school_email"].(string)
	sky := claimString(claims, "sky_number", "skyNumber")
	username := claimString(claims, "preferred_username")
	university := claimFirstString(claims, "university")
	department := claimFirstString(claims, "department")
	return Identity{
		ID: id,
		Profile: user.Profile{
			Email:       email,
			FirstName:   given,
			LastName:    family,
			Username:    username,
			SchoolEmail: school,
			SkyNumber:   sky,
			University:  university,
			Department:  department,
		},
		Groups: groupsFromClaims(claims),
		Roles:  rolesFromClaims(claims),
	}, claims, nil
}

func decodeJWTObject(parts []string, index int) (map[string]any, error) {
	if len(parts) != 3 || index < 0 || index >= len(parts) {
		return nil, ErrInvalidToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[index])
	if err != nil {
		return nil, ErrInvalidToken
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, ErrInvalidToken
	}
	return object, nil
}

func integerTimeClaim(raw any) (time.Time, bool) {
	number, ok := raw.(float64)
	if !ok || math.Trunc(number) != number || number < 0 || number > math.MaxInt64 {
		return time.Time{}, false
	}
	return time.Unix(int64(number), 0).UTC(), true
}

func audienceIncludes(raw any, want string) bool {
	switch v := raw.(type) {
	case string:
		return v == want
	case []any:
		for _, item := range v {
			s, ok := item.(string)
			if ok && s == want {
				return true
			}
		}
	case []string:
		for _, s := range v {
			if s == want {
				return true
			}
		}
	}
	return false
}

// audienceIsOnly reports whether an audience claim names want and nothing
// else. Keycloak serialises a single audience as a bare string or as a
// one-element array, so both shapes are read, but a second audience is a
// different token than the one this check asks for.
func audienceIsOnly(raw any, want string) bool {
	switch v := raw.(type) {
	case string:
		return v == want
	case []any:
		if len(v) != 1 {
			return false
		}
		s, ok := v[0].(string)
		return ok && s == want
	case []string:
		return len(v) == 1 && v[0] == want
	}
	return false
}

func groupsFromClaims(claims map[string]any) []string {
	raw, ok := claims["groups"]
	if !ok {
		raw = claims["group"]
	}
	switch v := raw.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })
	default:
		return nil
	}
}

func rolesFromClaims(claims map[string]any) []string {
	ra, ok := claims["resource_access"].(map[string]any)
	if !ok {
		return nil
	}
	ca, ok := ra[ResourceAudience].(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := ca["roles"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	seen := map[string]struct{}{}
	for _, item := range raw {
		s, ok := item.(string)
		if !ok || s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// claimFirstString reads a user-attribute claim that Keycloak writes as a bare
// string or, from a multivalued mapper, as an array; the first non-empty text
// value is the attribute.
func claimFirstString(claims map[string]any, key string) string {
	switch v := claims[key].(type) {
	case string:
		return v
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return ""
}

func claimString(claims map[string]any, keys ...string) string {
	for _, k := range keys {
		s, ok := claims[k].(string)
		if ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

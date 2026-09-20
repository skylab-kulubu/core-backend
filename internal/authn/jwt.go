package authn

import (
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
func ParseSelfDeleteContext(accessToken, idToken string, verify func(string) error, issuer, clientID string, now time.Time, maxAuthenticationAge time.Duration) (Identity, error) {
	if verify == nil || strings.TrimSpace(issuer) == "" || strings.TrimSpace(clientID) == "" || maxAuthenticationAge <= 0 {
		return Identity{}, ErrInvalidToken
	}
	if err := verify(accessToken); err != nil {
		return Identity{}, ErrInvalidToken
	}
	header, err := decodeJWTObject(strings.Split(strings.TrimSpace(accessToken), "."), 0)
	if err != nil || header["alg"] != "RS256" || header["typ"] != "JWT" {
		return Identity{}, ErrInvalidToken
	}
	ident, claims, err := decodeAccessToken(accessToken)
	if err != nil {
		return Identity{}, err
	}
	audience, ok := claims["aud"].(string)
	if !ok || audience != "account" || claims["iss"] != issuer || claims["azp"] != clientID || claims["scope"] != "openid" {
		return Identity{}, ErrInvalidToken
	}
	now = now.UTC()
	expiresAt, ok := integerTimeClaim(claims["exp"])
	if !ok || !expiresAt.After(now) {
		return Identity{}, ErrInvalidToken
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
	idAudience, ok := idClaims["aud"].(string)
	if !ok || idAudience != clientID || idClaims["iss"] != issuer {
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
	return Identity{
		ID: id,
		Profile: user.Profile{
			Email:       email,
			FirstName:   given,
			LastName:    family,
			Username:    username,
			SchoolEmail: school,
			SkyNumber:   sky,
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

func claimString(claims map[string]any, keys ...string) string {
	for _, k := range keys {
		s, ok := claims[k].(string)
		if ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

package authn

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

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

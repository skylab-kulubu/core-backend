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

func ParseAccessToken(token string) (Identity, error) {
	token = strings.TrimSpace(token)
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return Identity{}, ErrInvalidToken
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
			return Identity{}, ErrInvalidToken
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Identity{}, ErrInvalidToken
	}
	sub, _ := claims["sub"].(string)
	id, err := uuid.Parse(sub)
	if err != nil {
		return Identity{}, ErrInvalidToken
	}
	email, _ := claims["email"].(string)
	given, _ := claims["given_name"].(string)
	family, _ := claims["family_name"].(string)
	school, _ := claims["school_email"].(string)
	sky := claimString(claims, "sky_number", "skyNumber")
	return Identity{
		ID: id,
		Profile: user.Profile{
			Email:       email,
			FirstName:   given,
			LastName:    family,
			SchoolEmail: school,
			SkyNumber:   sky,
		},
		Groups: groupsFromClaims(claims),
		Roles:  rolesFromClaims(claims),
	}, nil
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
	out := make([]string, 0)
	seen := map[string]struct{}{}
	for _, client := range []string{"core", "skylapp"} {
		ca, ok := ra[client].(map[string]any)
		if !ok {
			continue
		}
		raw, ok := ca["roles"].([]any)
		if !ok {
			continue
		}
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

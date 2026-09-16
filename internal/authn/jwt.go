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
	return Identity{
		ID: id,
		Profile: user.Profile{
			Email:     email,
			FirstName: given,
			LastName:  family,
		},
		Groups: groupsFromClaims(claims),
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

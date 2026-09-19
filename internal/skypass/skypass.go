package skypass

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound  = errors.New("skypass: not found")
	ErrForbidden = errors.New("skypass: forbidden")
	ErrInvalid   = errors.New("skypass: invalid")
	ErrConflict  = errors.New("skypass: conflict")
	ErrExpired   = errors.New("skypass: expired")
)

const (
	Kid        = "sp-e1"
	DefaultTTL = 60 * time.Second
)

type Holder struct {
	ID        uuid.UUID `json:"id"`
	SkyNumber string    `json:"skyNumber"`
	FirstName string    `json:"firstName"`
	LastName  string    `json:"lastName"`
}

type Token struct {
	Value string    `json:"token"`
	Exp   time.Time `json:"exp"`
}

type JWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`
}

type JWKS struct {
	Keys []JWK `json:"keys"`
}

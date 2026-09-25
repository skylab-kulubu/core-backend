package media

import (
	"strings"
	"sync/atomic"
)

// DefaultPublicBase is the public base when none is configured: the
// production CDN.
const DefaultPublicBase = "https://cdn.yildizskylab.com"

// publicBase is the process's configured public base (UsePublicBase), read
// by every address built without a base of its own.
var publicBase atomic.Pointer[string]

// UsePublicBase makes base the public base of every address built without
// one: core sets it at startup from CDN_BASE (or R2_PUBLIC_URL), so moving
// the CDN is a configuration change. An empty base keeps DefaultPublicBase.
// The returned restore puts back the base it replaced.
func UsePublicBase(base string) (restore func()) {
	previous := publicBase.Load()
	if base = strings.TrimSpace(base); base != "" {
		publicBase.Store(&base)
	}
	return func() { publicBase.Store(previous) }
}

// configuredPublicBase is the base UsePublicBase set, or DefaultPublicBase.
func configuredPublicBase() string {
	if base := publicBase.Load(); base != nil {
		return *base
	}
	return DefaultPublicBase
}

// PublicURL is the public address of an object key under base, or under the
// configured public base when base is empty. A key that is already an
// absolute address is left alone.
func PublicURL(base, key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if isAbsoluteURL(key) {
		return key
	}
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = strings.TrimRight(configuredPublicBase(), "/")
	}
	return base + "/" + strings.TrimLeft(key, "/")
}

func isAbsoluteURL(s string) bool {
	if strings.HasPrefix(s, "//") {
		return true
	}
	scheme, rest, ok := strings.Cut(s, "://")
	if !ok || rest == "" || scheme == "" {
		return false
	}
	for i := 0; i < len(scheme); i++ {
		c := scheme[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '.' || c == '-'
		if !ok || (i == 0 && (c < 'a' || c > 'z')) {
			return false
		}
	}
	return true
}

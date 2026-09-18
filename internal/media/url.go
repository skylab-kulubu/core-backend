package media

import "strings"

const DefaultPublicBase = "https://cdn.yildizskylab.com"

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
		base = DefaultPublicBase
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

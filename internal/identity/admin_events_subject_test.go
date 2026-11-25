package identity

import (
	"encoding/base64"
	"testing"
)

func TestTokenSubject(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"5b0e3c1a-1111-4222-8333-944455556666","azp":"core"}`))
	id, err := tokenSubject("eyJhbGciOiJSUzI1NiJ9." + payload + ".sig")
	if err != nil || id.String() != "5b0e3c1a-1111-4222-8333-944455556666" {
		t.Fatalf("%v %v", id, err)
	}
	for _, bad := range []string{"tok", "a.b.c", "a." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"service-account-core"}`)) + ".c"} {
		if _, err := tokenSubject(bad); err == nil {
			t.Fatalf("%q: no error", bad)
		}
	}
}

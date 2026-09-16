package authn_test

import (
	"encoding/base64"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
)

func unsignedJWT(payload string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	body := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return header + "." + body + ".x"
}

func TestParseAccessTokenReadsGroups(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tok := unsignedJWT(`{"sub":"` + id.String() + `","email":"yk@example.com","given_name":"Y","family_name":"K","groups":["/UYELER/YK"]}`)
	got, err := authn.ParseAccessToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != id || got.Profile.Email != "yk@example.com" {
		t.Fatalf("got %+v", got)
	}
	if len(got.Groups) != 1 || got.Groups[0] != "/UYELER/YK" {
		t.Fatalf("groups %+v", got.Groups)
	}
}

func TestParseAccessTokenRejectsBadSub(t *testing.T) {
	t.Parallel()
	_, err := authn.ParseAccessToken(unsignedJWT(`{"sub":"not-a-uuid"}`))
	if err == nil {
		t.Fatal("expected error")
	}
}

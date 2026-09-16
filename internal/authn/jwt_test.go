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

func TestParseAccessTokenReadsSkyNumber(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tok := unsignedJWT(`{"sub":"` + id.String() + `","sky_number":"SKY-0000007"}`)
	got, err := authn.ParseAccessToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile.SkyNumber != "SKY-0000007" {
		t.Fatalf("profile %+v", got.Profile)
	}
}

func TestParseAccessTokenReadsSchoolEmail(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tok := unsignedJWT(`{"sub":"` + id.String() + `","email":"yk@example.com","school_email":"yk@std.yildiz.edu.tr"}`)
	got, err := authn.ParseAccessToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile.SchoolEmail != "yk@std.yildiz.edu.tr" {
		t.Fatalf("profile %+v", got.Profile)
	}
}

func TestParseAccessTokenReadsClientRoles(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tok := unsignedJWT(`{"sub":"` + id.String() + `","resource_access":{"core":{"roles":["url:create"]},"skylapp":{"roles":["skylapp:access","url:create"]}}}`)
	got, err := authn.ParseAccessToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Roles) != 2 {
		t.Fatalf("roles %+v", got.Roles)
	}
}

func TestParseAccessTokenRejectsBadSub(t *testing.T) {
	t.Parallel()
	_, err := authn.ParseAccessToken(unsignedJWT(`{"sub":"not-a-uuid"}`))
	if err == nil {
		t.Fatal("expected error")
	}
}

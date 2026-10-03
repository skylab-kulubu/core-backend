package authn_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
)

// The Group overage marker (ADR-0059) is Microsoft's: `_claim_names` names
// `groups` and `_claim_sources` says where the list is. Core looks only at
// the marker's presence, never at the endpoint in it.
func TestParseAccessTokenReadsTheGroupOverageMarker(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	marker := `"_claim_names":{"groups":"src1"},"_claim_sources":{"src1":{"endpoint":"https://evil.example/groups"}}`
	cases := []struct {
		name     string
		claims   string
		overage  bool
		wantPath string
	}{
		{name: "marker instead of the list", claims: marker, overage: true},
		// A list next to the marker is not the whole list: the marker wins.
		{name: "marker next to a list", claims: marker + `,"groups":["/UYELER/ARGE/WEBLAB"]`, overage: true},
		{name: "marker next to the singular claim", claims: marker + `,"group":"/UYELER/ARGE/WEBLAB"`, overage: true},
		{name: "marker without sources", claims: `"_claim_names":{"groups":"src1"}`, overage: true},
		{name: "list without marker", claims: `"groups":["/UYELER/ARGE/WEBLAB"]`, wantPath: "/UYELER/ARGE/WEBLAB"},
		{name: "another claim is distributed", claims: `"_claim_names":{"roles":"src1"},"groups":["/UYELER/ARGE/WEBLAB"]`, wantPath: "/UYELER/ARGE/WEBLAB"},
		{name: "claim names that are not an object", claims: `"_claim_names":"groups","groups":["/UYELER/ARGE/WEBLAB"]`, wantPath: "/UYELER/ARGE/WEBLAB"},
		{name: "no groups at all", claims: `"email":"x@example.com"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := authn.ParseAccessToken(unsignedJWT(`{"sub":"` + id.String() + `",` + tc.claims + `}`))
			if err != nil {
				t.Fatal(err)
			}
			if got.GroupOverage != tc.overage {
				t.Fatalf("GroupOverage = %v, want %v", got.GroupOverage, tc.overage)
			}
			if tc.overage && got.Groups != nil {
				t.Fatalf("an overage token answered Groups %q; its list is not the person's", got.Groups)
			}
			if tc.wantPath != "" && (len(got.Groups) != 1 || got.Groups[0] != tc.wantPath) {
				t.Fatalf("Groups = %q, want [%q]", got.Groups, tc.wantPath)
			}
		})
	}
}

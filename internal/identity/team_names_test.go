package identity

import (
	"context"
	"errors"
	"testing"
)

type listedGroups struct {
	groups []Group
	err    error
}

func (l listedGroups) ListGroups(context.Context) ([]Group, error) { return l.groups, l.err }

func TestTurkishTeamNames(t *testing.T) {
	t.Parallel()
	read := TurkishTeamNames(listedGroups{groups: []Group{
		{Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB", Attributes: map[string]string{"display_name_tr": " Web Geliştirme "}},
		{Name: "GECEKODU", Path: "/UYELER/GECEKODU"},
		{Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/LIDERLER", Attributes: map[string]string{"display_name_tr": "Liderler"}},
		{Name: "WEBLAB", Path: "/BASKA/WEBLAB", Attributes: map[string]string{"display_name_tr": "Başka"}},
	}})
	got, err := read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got["WEBLAB"] != "Web Geliştirme" || got["LIDERLER"] != "Liderler" || len(got) != 2 {
		t.Fatalf("names %v", got)
	}
	if _, err := TurkishTeamNames(listedGroups{err: errors.New("down")})(context.Background()); err == nil {
		t.Fatal("no error")
	}
}

package membershipterm

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// A Team's Leader group is named by its Team: LIDERLER alone does not say
// whose. Every other Group keeps its own name.
func TestTheMailsNameALeaderGroupByItsTeam(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	for _, g := range []identity.Group{
		{ID: "yk", Name: "YK", Path: "/UYELER/YK", Attributes: map[string]string{"display_name_tr": "Yönetim Kurulu"}},
		{ID: "weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB", Attributes: map[string]string{"display_name_tr": "Web Geliştirme"}},
		{ID: "weblab-l", Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/LIDERLER", Attributes: map[string]string{"display_name_tr": "Liderler"}},
		{ID: "ai", Name: "AI", Path: "/UYELER/ARGE/WEBLAB/AI"},
		{ID: "ai-k", Name: "KOORDINATORLER", Path: "/UYELER/ARGE/WEBLAB/AI/KOORDINATORLER"},
	} {
		dir.PutGroup(g)
	}
	s := &Service{config: Config{Directory: dir, Rules: DefaultRules()}}
	ctx := context.Background()
	for ref, want := range map[string]string{
		"yk":       "YK · Yönetim Kurulu",
		"weblab-l": "WEBLAB/LIDERLER · Web Geliştirme",
		"ai-k":     "AI/KOORDINATORLER",
	} {
		if got := s.groupName(ctx, s.group(ctx, ref, "")); got != want {
			t.Errorf("%s named %q, want %q", ref, got, want)
		}
	}
	// A Leader group gone since, and its Team too: named by the paths.
	gone := s.group(ctx, "missing", "/UYELER/ARGE/OYUN/LIDERLER")
	if got := s.groupName(ctx, gone); got != "OYUN/LIDERLER" {
		t.Errorf("a gone Leader group named %q", got)
	}
}

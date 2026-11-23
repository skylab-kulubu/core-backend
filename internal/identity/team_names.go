package identity

import (
	"context"
	"strings"
)

// TurkishTeamNames reads the Groups and answers each code (a Group's name)
// with its Turkish name (display_name_tr): what organizer.Names caches for
// the organizer of an Event (Yusuf, 2026-11-23). A code named twice in the
// tree keeps the first name found; a Group without one is left out.
func TurkishTeamNames(dir GroupLister) func(ctx context.Context) (map[string]string, error) {
	return func(ctx context.Context) (map[string]string, error) {
		groups, err := dir.ListGroups(ctx)
		if err != nil {
			return nil, err
		}
		out := make(map[string]string, len(groups))
		for _, g := range groups {
			code := strings.TrimSpace(g.Name)
			turkish := strings.TrimSpace(g.Attributes["display_name_tr"])
			if code == "" || turkish == "" {
				continue
			}
			if _, seen := out[code]; !seen {
				out[code] = turkish
			}
		}
		return out, nil
	}
}

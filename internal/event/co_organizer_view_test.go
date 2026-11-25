package event_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Anyone sees the Co-organizer teams shown outside the panel, by code and
// by name, in the Event JSON and its summary (Resource); a helper team and a
// person never; an Event without them answers neither field.
func TestPublicCoOrganizerNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newListWorld(t)
	got, err := w.svc.Get(ctx, nil, w.published.ID)
	if err != nil {
		t.Fatal(err)
	}
	public := w.svc.ProjectFor(nil, got)
	raw, _ := json.Marshal(public)
	if public.CoOrganizerNames != "WEBLAB" || !strings.Contains(string(raw), `"coOrganizerTeams":["WEBLAB"]`) ||
		strings.Contains(string(raw), "GECEKODU") || strings.Contains(string(raw), w.personID.String()) {
		t.Fatalf("public JSON %s", raw)
	}
	summary, _ := json.Marshal(got.Resource())
	if !strings.Contains(string(summary), `"coOrganizerTeams":["WEBLAB"],"coOrganizerNames":"WEBLAB"`) || strings.Contains(string(summary), "GECEKODU") {
		t.Fatalf("summary %s", summary)
	}
	other, err := w.svc.Get(ctx, nil, w.other.ID)
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]any{"JSON": w.svc.ProjectFor(nil, other), "summary": other.Resource()} {
		if raw, _ := json.Marshal(v); strings.Contains(string(raw), "coOrganizer") {
			t.Fatalf("%s without Co-organizers %s", name, raw)
		}
	}
}

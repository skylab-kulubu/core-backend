package handlers

import (
	"reflect"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// partialFull is an Event with every field the update body carries set.
const partialFull = `{
	"name":"Gecekodu","description":"Açıklama","location":"YTÜ","ownerTeam":"WEBLAB",
	"formUrl":"https://forms.yildizskylab.com/f/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","formAlias":"gecekodu",
	"extraFormUrls":[{"label":"CTF","url":"https://forms.yildizskylab.com/f/bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","alias":"ctf"}],
	"capacity":120,"startDate":"2030-03-14T16:00:00Z","endDate":"2030-03-15T00:00:00Z",
	"linkedin":"https://linkedin.com/x","ranked":true,"prizeInfo":"Ödül",
	"coverImageId":"cccccccc-cccc-cccc-cccc-cccccccccccc",
	"attendanceRule":"ratio","attendanceRatio":0.5,
	"doorStaffIds":["dddddddd-dddd-dddd-dddd-dddddddddddd"]}`

// updatedFields are the fields an update itself changes.
var updatedFields = []string{"updatedAt"}

func withoutFields(in map[string]any, keys ...string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	for _, k := range append(keys, updatedFields...) {
		delete(out, k)
	}
	return out
}

func createPartialEvent(t *testing.T, app *fiber.App) map[string]any {
	t.Helper()
	status, created := sendEventJSON(t, app, fiber.MethodPost, "/v1/events", partialFull)
	if status != fiber.StatusCreated {
		t.Fatalf("create %d %v", status, created)
	}
	return created
}

func TestEventUpdateKeepsEveryFieldTheBodyLeavesOut(t *testing.T) {
	t.Parallel()
	for _, method := range []string{fiber.MethodPut, fiber.MethodPatch} {
		app := eventApp(t, yk(), event.NewMemoryStore())
		created := createPartialEvent(t, app)
		path := "/v1/events/" + created["id"].(string)

		status, updated := sendEventJSON(t, app, method, path, `{"description":"Yeni"}`)
		if status != fiber.StatusOK {
			t.Fatalf("%s %d %v", method, status, updated)
		}
		if updated["description"] != "Yeni" {
			t.Fatalf("%s description %v", method, updated["description"])
		}
		if got, want := withoutFields(updated, "description"), withoutFields(created, "description"); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s changed a field it was not sent:\n got %v\nwant %v", method, got, want)
		}

		// An empty body changes nothing, and status and active are not read.
		status, same := sendEventJSON(t, app, method, path, `{"status":"draft","active":false,"id":"eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"}`)
		if status != fiber.StatusOK {
			t.Fatalf("%s empty %d %v", method, status, same)
		}
		if got, want := withoutFields(same), withoutFields(updated); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s empty body changed the Event:\n got %v\nwant %v", method, got, want)
		}
	}
}

func TestEventUpdateWritesSentValuesAndClearsNull(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		// want is the answered field values; nil means the key is left out
		// (cleared).
		want map[string]any
	}{
		{"name", `{"name":"Gecekodu 2"}`, map[string]any{"name": "Gecekodu 2"}},
		{"location", `{"location":"Davutpaşa"}`, map[string]any{"location": "Davutpaşa"}},
		{"description cleared", `{"description":null}`, map[string]any{"description": ""}},
		{"description empty", `{"description":""}`, map[string]any{"description": ""}},
		{"formUrl cleared", `{"formUrl":null}`, map[string]any{"formUrl": nil}},
		{"formAlias cleared", `{"formAlias":""}`, map[string]any{"formAlias": nil}},
		{"extraFormUrls cleared", `{"extraFormUrls":null}`, map[string]any{"extraFormUrls": []any{}}},
		{"extraFormUrls empty", `{"extraFormUrls":[]}`, map[string]any{"extraFormUrls": []any{}}},
		{"capacity", `{"capacity":30}`, map[string]any{"capacity": float64(30)}},
		{"capacity cleared", `{"capacity":null}`, map[string]any{"capacity": float64(0)}},
		{"startDate", `{"startDate":"2031-01-01T10:00:00Z"}`, map[string]any{"startDate": "2031-01-01T10:00:00Z"}},
		{"startDate cleared", `{"startDate":null}`, map[string]any{"startDate": nil}},
		{"endDate cleared", `{"endDate":null}`, map[string]any{"endDate": nil}},
		{"linkedin cleared", `{"linkedin":""}`, map[string]any{"linkedin": nil}},
		{"ranked off", `{"ranked":false}`, map[string]any{"ranked": false}},
		{"prizeInfo cleared", `{"prizeInfo":null}`, map[string]any{"prizeInfo": nil}},
		{"cover cleared", `{"coverImageId":null}`, map[string]any{"coverImageId": nil}},
		{"attendance none", `{"attendanceRule":"none","attendanceRatio":null}`, map[string]any{"attendanceRule": "none", "attendanceRatio": nil}},
		{"attendance rule cleared", `{"attendanceRule":null}`, map[string]any{"attendanceRule": "none"}},
		{"attendance ratio", `{"attendanceRatio":0.75}`, map[string]any{"attendanceRule": "ratio", "attendanceRatio": 0.75}},
		{"door staff cleared", `{"doorStaffIds":null}`, map[string]any{"doorStaffIds": nil}},
		{"door staff", `{"doorStaffIds":["ffffffff-ffff-ffff-ffff-ffffffffffff"]}`, map[string]any{"doorStaffIds": []any{"ffffffff-ffff-ffff-ffff-ffffffffffff"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := eventApp(t, yk(), event.NewMemoryStore())
			created := createPartialEvent(t, app)
			status, updated := sendEventJSON(t, app, fiber.MethodPatch, "/v1/events/"+created["id"].(string), tc.body)
			if status != fiber.StatusOK {
				t.Fatalf("%d %v", status, updated)
			}
			changed := make([]string, 0, len(tc.want))
			for key, want := range tc.want {
				changed = append(changed, key)
				got, present := updated[key]
				if want == nil {
					if present {
						t.Fatalf("%s = %v, want it cleared", key, got)
					}
					continue
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s = %#v, want %#v", key, got, want)
				}
			}
			if got, want := withoutFields(updated, changed...), withoutFields(created, changed...); !reflect.DeepEqual(got, want) {
				t.Fatalf("changed a field it was not sent:\n got %v\nwant %v", got, want)
			}
		})
	}
}

func TestEventUpdateRefusesClearingARequiredField(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"name":""}`, `{"name":null}`, `{"name":"  "}`,
		`{"location":""}`, `{"location":null}`,
		`{"ownerTeam":""}`, `{"ownerTeam":null}`,
		`{"ranked":null}`,
		`{"attendanceRule":"ratio","attendanceRatio":null}`,
		`{"capacity":"çok"}`,
	} {
		app := eventApp(t, yk(), event.NewMemoryStore())
		created := createPartialEvent(t, app)
		path := "/v1/events/" + created["id"].(string)
		for _, method := range []string{fiber.MethodPut, fiber.MethodPatch} {
			if status, out := sendEventJSON(t, app, method, path, body); status != fiber.StatusBadRequest {
				t.Fatalf("%s %s: %d %v", method, body, status, out)
			}
		}
		_, after := sendEventJSON(t, app, fiber.MethodGet, path, "")
		if got, want := withoutFields(after), withoutFields(created); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: a refused update changed the Event:\n got %v\nwant %v", body, got, want)
		}
	}
}

// TestEventUpdateWholeBodyAsSkyApp: sky-app sends every field it shows and
// the ones it keeps from the read Event, but never the forms' aliases,
// the extra forms or the door staff; the answer is what it was before
// ticket 13.
func TestEventUpdateWholeBodyAsSkyApp(t *testing.T) {
	t.Parallel()
	app := eventApp(t, yk(), event.NewMemoryStore())
	created := createPartialEvent(t, app)
	path := "/v1/events/" + created["id"].(string)
	status, updated := sendEventJSON(t, app, fiber.MethodPut, path, `{
		"name":"Gecekodu","location":"Davutpaşa","ownerTeam":"WEBLAB",
		"startDate":"2030-03-14T16:00:00Z","endDate":"2030-03-15T00:00:00Z",
		"description":"Açıklama","formUrl":"https://forms.yildizskylab.com/f/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"linkedin":"https://linkedin.com/x","capacity":120,
		"coverImageId":"cccccccc-cccc-cccc-cccc-cccccccccccc","ranked":true,"prizeInfo":"Ödül",
		"attendanceRule":"ratio","attendanceRatio":0.5,"active":false}`)
	if status != fiber.StatusOK {
		t.Fatalf("%d %v", status, updated)
	}
	if got, want := withoutFields(updated, "location"), withoutFields(created, "location"); !reflect.DeepEqual(got, want) {
		t.Fatalf("sky-app's whole body changed more than the location:\n got %v\nwant %v", got, want)
	}
	if updated["location"] != "Davutpaşa" || updated["active"] != true || updated["status"] != "published" {
		t.Fatalf("updated %v", updated)
	}
}

func TestEventUpdateKeepsMemberOut(t *testing.T) {
	t.Parallel()
	store := event.NewMemoryStore()
	created := createPartialEvent(t, eventApp(t, yk(), store))
	path := "/v1/events/" + created["id"].(string)
	leader := eventApp(t, weblabLeader(), store)
	// Another team needs the right on it too, and a refused body is 403
	// before it is 400.
	if status, out := sendEventJSON(t, leader, fiber.MethodPatch, path, `{"ownerTeam":"AIRLAB"}`); status != fiber.StatusForbidden {
		t.Fatalf("move to another team %d %v", status, out)
	}
	member := eventApp(t, partialMember(), store)
	if status, out := sendEventJSON(t, member, fiber.MethodPatch, path, `{"name":""}`); status != fiber.StatusForbidden {
		t.Fatalf("member %d %v", status, out)
	}
}

func partialMember() authn.Identity {
	return authn.Identity{
		ID:     uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		Groups: []string{"/UYELER/ARGE/WEBLAB"},
	}
}

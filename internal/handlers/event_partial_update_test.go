package handlers

import (
	"encoding/json"
	"net/http/httptest"
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
	"locationLat":41.025376,"locationLng":28.889648,"locationAddress":"Davutpaşa Cd. No:127, Esenler",
	"formUrl":"https://forms.yildizskylab.com/f/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","formAlias":"gecekodu",
	"extraFormUrls":[{"label":"CTF","url":"https://forms.yildizskylab.com/f/bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","alias":"ctf"}],
	"capacity":120,"startDate":"2030-03-14T16:00:00Z","endDate":"2030-03-15T00:00:00Z",
	"linkedin":"https://linkedin.com/x","ranked":true,"prizeInfo":"Ödül",
	"coverImageId":"cccccccc-cccc-cccc-cccc-cccccccccccc",
	"attendanceRule":"count","attendanceMinCount":6,
	"doorStaffIds":["dddddddd-dddd-dddd-dddd-dddddddddddd"],
	"feedbackMode":"per_session","feedbackFormUrl":"https://forms.yildizskylab.com/f/eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee",
	"feedbackFormAlias":"gecekodu-geri-bildirim","feedbackAutoSend":false}`

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
		{"point moved", `{"locationLat":41.0431234567,"locationLng":29.009}`, map[string]any{"locationLat": 41.043123, "locationLng": 29.009}},
		{"point cleared", `{"locationLat":null,"locationLng":null}`, map[string]any{"locationLat": nil, "locationLng": nil}},
		{"address", `{"locationAddress":"Beşiktaş"}`, map[string]any{"locationAddress": "Beşiktaş"}},
		{"address cleared", `{"locationAddress":null}`, map[string]any{"locationAddress": nil}},
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
		{"attendance none", `{"attendanceRule":"none"}`, map[string]any{"attendanceRule": "none", "attendanceMinCount": nil}},
		{"attendance rule cleared", `{"attendanceRule":null}`, map[string]any{"attendanceRule": "none", "attendanceMinCount": nil}},
		{"attendance once drops the threshold", `{"attendanceRule":"once","attendanceMinCount":3}`, map[string]any{"attendanceRule": "once", "attendanceMinCount": nil}},
		{"attendance threshold", `{"attendanceMinCount":4}`, map[string]any{"attendanceMinCount": float64(4)}},
		{"attendance threshold default", `{"attendanceMinCount":null}`, map[string]any{"attendanceMinCount": nil}},
		{"door staff cleared", `{"doorStaffIds":null}`, map[string]any{"doorStaffIds": nil}},
		{"door staff", `{"doorStaffIds":["ffffffff-ffff-ffff-ffff-ffffffffffff"]}`, map[string]any{"doorStaffIds": []any{"ffffffff-ffff-ffff-ffff-ffffffffffff"}}},
		{"feedback mode", `{"feedbackMode":"event_with_sessions"}`, map[string]any{"feedbackMode": "event_with_sessions"}},
		{"feedback form cleared", `{"feedbackFormUrl":null,"feedbackFormAlias":null}`, map[string]any{"feedbackFormUrl": nil, "feedbackFormAlias": nil}},
		{"feedback sent automatically", `{"feedbackAutoSend":true}`, map[string]any{"feedbackAutoSend": true}},
		{"attendance closed by hand only", `{"attendanceAutoFinalize":false}`, map[string]any{"attendanceAutoFinalize": false}},
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
		`{"feedbackMode":null}`, `{"feedbackMode":"herkes"}`, `{"feedbackAutoSend":null}`, `{"attendanceAutoFinalize":null}`,
		`{"attendanceRule":"ratio","attendanceRatio":null}`,
		`{"attendanceRule":"ratio","attendanceRatio":0.75}`,
		`{"attendanceMinCount":0}`, `{"attendanceRule":"count","attendanceMinCount":-1}`,
		`{"capacity":"çok"}`,
		`{"locationLat":41.05}`, `{"locationLng":null}`, `{"locationLat":95,"locationLng":29}`,
		`{"locationLat":"kuzey","locationLng":29}`,
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
		"attendanceRule":"count","active":false}`)
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

// A point the store cannot keep answers its own code, for the panel to
// show on the place's fields; so does one on create.
func TestEventLocationRefusalHasItsCode(t *testing.T) {
	t.Parallel()
	app := eventApp(t, yk(), event.NewMemoryStore())
	created := createPartialEvent(t, app)
	status, out := sendEventJSON(t, app, fiber.MethodPatch, "/v1/events/"+created["id"].(string), `{"locationLat":41.05}`)
	if status != fiber.StatusBadRequest || out["code"] != "event_location_invalid" {
		t.Fatalf("patch %d %v", status, out)
	}
	status, out = sendEventJSON(t, app, fiber.MethodPost, "/v1/events",
		`{"name":"Gecekodu","location":"YTÜ","ownerTeam":"WEBLAB","locationLat":0,"locationLng":0}`)
	if status != fiber.StatusBadRequest || out["code"] != "event_location_invalid" {
		t.Fatalf("create %d %v", status, out)
	}
}

// GET /v1/events/places: a team's places for its leader, the club's for
// YK; signed out 401, a bad limit 400, the club's for a leader 403.
func TestEventPlacesRoute(t *testing.T) {
	t.Parallel()
	store := event.NewMemoryStore()
	createPartialEvent(t, eventApp(t, yk(), store))
	leader := eventApp(t, weblabLeader(), store)
	resp, err := leader.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events/places?ownerTeam=WEBLAB", nil))
	if err != nil {
		t.Fatal(err)
	}
	var places []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&places); err != nil || resp.StatusCode != fiber.StatusOK || len(places) != 1 ||
		places[0]["name"] != "YTÜ" || places[0]["address"] != "Davutpaşa Cd. No:127, Esenler" || places[0]["lat"] != 41.025376 || places[0]["uses"] != float64(1) {
		t.Fatalf("%d %v %v", resp.StatusCode, places, err)
	}
	for _, tc := range []struct {
		app    *fiber.App
		path   string
		status int
	}{
		{eventApp(t, authn.Identity{}, store), "/v1/events/places?ownerTeam=WEBLAB", fiber.StatusUnauthorized},
		{leader, "/v1/events/places", fiber.StatusForbidden},
		{leader, "/v1/events/places?ownerTeam=WEBLAB&limit=0", fiber.StatusBadRequest},
		{leader, "/v1/events/places?ownerTeam=WEBLAB&limit=çok", fiber.StatusBadRequest},
		{eventApp(t, yk(), store), "/v1/events/places?limit=50", fiber.StatusOK},
	} {
		resp, err := tc.app.Test(httptest.NewRequest(fiber.MethodGet, tc.path, nil))
		if err != nil || resp.StatusCode != tc.status {
			t.Errorf("%s: %v %v", tc.path, resp.StatusCode, err)
		}
	}
}

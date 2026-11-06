package events

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// What keycloak-to-rabbitmq 3.1.1 sends today: Keycloak's Event with the IP
// address, the session and the sign-up's details, and no id.
const keycloakRegisterToday = `{"@class":"com.github.aznamier.keycloak.event.provider.EventClientNotificationMqMsg",` +
	`"id":null,"time":1761213600123,"type":"REGISTER","realmId":"e-skylab","clientId":"account",` +
	`"userId":"5f7c2a8e-0000-4000-8000-000000000001","sessionId":"s-1","ipAddress":"203.0.113.7",` +
	`"error":null,"details":{"username":"ayse","email":"ayse@example.com","register_method":"form"}}`

// What SKY LAB's fork sends (hub ticket R07): the personal fields left out,
// Keycloak's event id.
const keycloakRegisterFiltered = `{"id":"0b5e8f9e-1111-4111-8111-000000000002","time":1761213600123,"type":"REGISTER",` +
	`"realmId":"e-skylab","clientId":"account","userId":"5f7c2a8e-0000-4000-8000-000000000001"}`

// An admin's change to a user, with the representation an admin event may
// carry and the admin's auth details.
const keycloakAdminUpdate = `{"@class":"com.github.aznamier.keycloak.event.provider.EventAdminNotificationMqMsg",` +
	`"time":1761213600999,"realmId":"e-skylab","authDetails":{"realmId":"master","clientId":"admin-cli",` +
	`"userId":"a-1","ipAddress":"198.51.100.4"},"resourceType":"USER","operationType":"UPDATE",` +
	`"resourcePath":"users/5f7c2a8e-0000-4000-8000-000000000001","representation":"{\"email\":\"ayse@example.com\"}","error":null}`

// What SKY LAB's fork sends for a deleted Group (hub ticket rabbitmq-olaylar
// 05): ids only. realmId is Keycloak's id of the realm, not its name.
const keycloakGroupDelete = `{"id":"7d2f0c55-3333-4333-8333-000000000004","realmId":"5b0e2d9a-2222-4222-8222-000000000003",` +
	`"resourceType":"GROUP","operationType":"DELETE","resourcePath":"groups/9a1c4e7b-4444-4444-8444-000000000005","time":1761213601000}`

func TestDecodeKeycloak(t *testing.T) {
	for name, tc := range map[string]struct {
		message Message
		id      string
		typ     string
		want    KeycloakEvent
	}{
		"today's provider: no id, so the body's hash": {
			message: Message{Body: []byte(keycloakRegisterToday), ContentType: "application/json"},
			typ:     "keycloak.register",
			want:    KeycloakEvent{Type: "REGISTER", RealmID: "e-skylab", ClientID: "account", UserID: "5f7c2a8e-0000-4000-8000-000000000001"},
		},
		"the fork's: Keycloak's id": {
			message: Message{Body: []byte(keycloakRegisterFiltered), ContentType: "application/json", MessageID: "ignored"},
			id:      "0b5e8f9e-1111-4111-8111-000000000002",
			typ:     "keycloak.register",
			want:    KeycloakEvent{Type: "REGISTER", RealmID: "e-skylab", ClientID: "account", UserID: "5f7c2a8e-0000-4000-8000-000000000001"},
		},
		"no id in the body: the message-id": {
			message: Message{Body: []byte(`{"type":"UPDATE_EMAIL","userId":"u-1"}`), MessageID: "m-1"},
			id:      "m-1",
			typ:     "keycloak.update_email",
			want:    KeycloakEvent{Type: "UPDATE_EMAIL", UserID: "u-1"},
		},
		"an admin event": {
			message: Message{Body: []byte(keycloakAdminUpdate), ContentType: "application/json; charset=utf-8"},
			typ:     "keycloak.admin.user.update",
			want: KeycloakEvent{Admin: true, ResourceType: "USER", OperationType: "UPDATE", RealmID: "e-skylab",
				ResourcePath: "users/5f7c2a8e-0000-4000-8000-000000000001"},
		},
		"an admin's deleted group, with the realm of its routing key": {
			message: Message{Body: []byte(keycloakGroupDelete), ContentType: "application/json", RoutingKey: "KK.EVENT.ADMIN.e-skylab.SUCCESS.GROUP.DELETE"},
			id:      "7d2f0c55-3333-4333-8333-000000000004",
			typ:     "keycloak.admin.group.delete",
			want: KeycloakEvent{Admin: true, ResourceType: "GROUP", OperationType: "DELETE", RealmID: "5b0e2d9a-2222-4222-8222-000000000003",
				Realm: "e-skylab", ResourcePath: "groups/9a1c4e7b-4444-4444-8444-000000000005"},
		},
		"a user event's realm from its routing key": {
			message: Message{Body: []byte(keycloakRegisterFiltered), RoutingKey: "KK.EVENT.CLIENT.e-skylab.SUCCESS.account.REGISTER"},
			id:      "0b5e8f9e-1111-4111-8111-000000000002",
			typ:     "keycloak.register",
			want:    KeycloakEvent{Type: "REGISTER", RealmID: "e-skylab", Realm: "e-skylab", ClientID: "account", UserID: "5f7c2a8e-0000-4000-8000-000000000001"},
		},
		"a failed event": {
			message: Message{Body: []byte(`{"id":"e-1","type":"REGISTER","error":"invalid_registration"}`)},
			id:      "e-1",
			typ:     "keycloak.register",
			want:    KeycloakEvent{Type: "REGISTER", Failed: true},
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, err := DecodeKeycloak(tc.message)
			if err != nil {
				t.Fatal(err)
			}
			if e.Source != KeycloakSource || e.Type != tc.typ {
				t.Fatalf("event = %+v", e)
			}
			switch {
			case tc.id != "" && e.ID != tc.id:
				t.Fatalf("id = %q, want %q", e.ID, tc.id)
			case tc.id == "" && !strings.HasPrefix(e.ID, "sha256:"):
				t.Fatalf("id = %q, want the body's hash", e.ID)
			}
			kc, err := KeycloakOf(e)
			if err != nil {
				t.Fatal(err)
			}
			if kc != tc.want {
				t.Fatalf("kept %+v, want %+v", kc, tc.want)
			}
			// Nothing personal is kept: not the IP, the details, the
			// session or the representation.
			for _, gone := range []string{"203.0.113.7", "198.51.100.4", "ayse", "s-1", "representation", "details"} {
				if strings.Contains(string(e.Data), gone) {
					t.Fatalf("data keeps %q: %s", gone, e.Data)
				}
			}
		})
	}
}

// Only a users/<id> or groups/<id> resource path is kept; any other is left
// out, and the event is still read.
func TestDecodeKeycloakResourcePath(t *testing.T) {
	const id = "9a1c4e7b-4444-4444-8444-000000000005"
	for path, want := range map[string]string{
		"users/" + id:                          "users/" + id,
		"groups/" + strings.ToUpper(id):        "groups/" + strings.ToUpper(id),
		"groups/" + id + "/children":           "",
		"users/" + id + "/groups/" + id:        "",
		"clients/" + id:                        "",
		"groups/not-an-id":                     "",
		"groups/" + id + "0":                   "",
		" groups/" + id:                        "groups/" + id,
		"groups/ayse@example.com-000000000000": "",
		"":                                     "",
	} {
		body := `{"id":"e","resourceType":"GROUP","operationType":"DELETE","resourcePath":` + strconv.Quote(path) + `}`
		e, err := DecodeKeycloak(Message{Body: []byte(body)})
		if err != nil {
			t.Fatalf("%q: %v", path, err)
		}
		kc, err := KeycloakOf(e)
		if err != nil || kc.ResourcePath != want {
			t.Fatalf("%q: kept %q (%v), want %q", path, kc.ResourcePath, err, want)
		}
		if want == "" && path != "" && strings.Contains(string(e.Data), strings.TrimSpace(path)) {
			t.Fatalf("%q: data keeps it: %s", path, e.Data)
		}
	}
}

// The realm is the routing key's fourth word (the provider takes the dots
// out of the realm's name); a key that is not the provider's leaves it out.
func TestDecodeKeycloakRealm(t *testing.T) {
	for key, want := range map[string]string{
		"KK.EVENT.ADMIN.e-skylab.SUCCESS.USER.DELETE":               "e-skylab",
		"KK.EVENT.CLIENT.e-skylab-sandbox.SUCCESS.account.REGISTER": "e-skylab-sandbox",
		"KK.EVENT.ADMIN": "",
		"core.keycloak":  "",
		"":               "",
	} {
		e, err := DecodeKeycloak(Message{Body: []byte(keycloakGroupDelete), RoutingKey: key})
		if err != nil {
			t.Fatal(err)
		}
		if kc, _ := KeycloakOf(e); kc.Realm != want {
			t.Fatalf("%q: realm %q, want %q", key, kc.Realm, want)
		}
	}
}

func TestDecodeKeycloakTimeAndRepeats(t *testing.T) {
	m := Message{Body: []byte(keycloakRegisterToday)}
	first, err := DecodeKeycloak(m)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.UnixMilli(1761213600123).UTC(); !first.Time.Equal(want) {
		t.Fatalf("time = %v, want %v", first.Time, want)
	}
	// The same body is the same event: its id does not change.
	again, _ := DecodeKeycloak(m)
	if again.ID != first.ID {
		t.Fatalf("ids %q and %q", first.ID, again.ID)
	}
	// A time that is not Keycloak's milliseconds is left out, not refused.
	odd, err := DecodeKeycloak(Message{Body: []byte(`{"id":"e","type":"REGISTER","time":"yesterday"}`)})
	if err != nil || !odd.Time.IsZero() {
		t.Fatalf("time %v, err %v", odd.Time, err)
	}
}

func TestDecodeKeycloakRefuses(t *testing.T) {
	for name, m := range map[string]Message{
		"not JSON":             {Body: []byte(`not json`)},
		"two values":           {Body: []byte(`{"type":"REGISTER"} {}`)},
		"no type":              {Body: []byte(`{"id":"e","userId":"u"}`)},
		"null":                 {Body: []byte(`null`)},
		"a type of lower case": {Body: []byte(`{"id":"e","type":"register"}`)},
		"half an admin event":  {Body: []byte(`{"id":"e","resourceType":"USER"}`)},
		"a number for a type":  {Body: []byte(`{"id":"e","type":7}`)},
		"another content type": {Body: []byte(keycloakRegisterFiltered), ContentType: CloudEventsContentType},
		"too large":            {Body: []byte(`{"type":"REGISTER","x":"` + strings.Repeat("a", MaxBody) + `"}`)},
		"a long id":            {Body: []byte(`{"id":"` + strings.Repeat("a", 256) + `","type":"REGISTER"}`)},
		"a long user id":       {Body: []byte(`{"id":"e","type":"REGISTER","userId":"` + strings.Repeat("a", 256) + `"}`)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeKeycloak(m); err == nil {
				t.Fatal("decoded")
			}
		})
	}
}

func TestKeycloakOfOtherEvents(t *testing.T) {
	e, err := DecodeCloudEvent(Message{Body: []byte(event("1"))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := KeycloakOf(e); err == nil {
		t.Fatal("read a CloudEvent as a Keycloak event")
	}
}

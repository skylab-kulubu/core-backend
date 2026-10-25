package events

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// KeycloakSource is the source of every event DecodeKeycloak reads: with the
// event's id it is the event's record in consumed_events.
const KeycloakSource = "/keycloak"

// keycloakToken is a Keycloak event type, resource type or operation type
// (REGISTER, USER, CREATE): the provider puts them in its routing keys.
var keycloakToken = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// KeycloakEvent is what core keeps of a Keycloak event: kinds and ids, never
// the IP address, the details (a sign-up's e-mail, a change's old and new
// address), the session or an admin event's representation, which the
// provider may still send.
type KeycloakEvent struct {
	// Admin is an admin event (KK.EVENT.ADMIN…): ResourceType and
	// OperationType say what changed. Otherwise a user event (KK.EVENT.CLIENT…)
	// whose Type says what happened.
	Admin         bool   `json:"admin,omitempty"`
	Type          string `json:"type,omitempty"`
	ResourceType  string `json:"resourceType,omitempty"`
	OperationType string `json:"operationType,omitempty"`
	RealmID       string `json:"realmId,omitempty"`
	ClientID      string `json:"clientId,omitempty"`
	UserID        string `json:"userId,omitempty"`
	// Failed is an event Keycloak sent with an error (…ERROR… in the routing
	// key): nothing changed.
	Failed bool `json:"failed,omitempty"`
}

// keycloakBody is the provider's JSON (Keycloak's Event or AdminEvent):
// keycloak-to-rabbitmq as it is, or SKY LAB's fork that leaves out the
// personal fields (hub ticket R07). Fields core does not read are ignored.
type keycloakBody struct {
	ID            *string         `json:"id"`
	Type          *string         `json:"type"`
	RealmID       *string         `json:"realmId"`
	ClientID      *string         `json:"clientId"`
	UserID        *string         `json:"userId"`
	Time          json.RawMessage `json:"time"`
	Error         *string         `json:"error"`
	ResourceType  *string         `json:"resourceType"`
	OperationType *string         `json:"operationType"`
}

// DecodeKeycloak reads an event of Keycloak's RabbitMQ provider (ADR-0071
// §5): Keycloak's own Event or AdminEvent JSON, not CloudEvents. An admin
// event has an operationType and a resourceType, a user event a type; each
// is an upper-case Keycloak name. The id is the event's id, else the
// message-id, else the body's SHA-256 (the provider before R07 sends none:
// the same body is the same event). time, when it is Keycloak's
// milliseconds, is kept; any other field, and every personal one (ipAddress,
// details, sessionId, representation, authDetails), is left out. The AMQP
// content-type, when set, must be application/json.
func DecodeKeycloak(m Message) (Event, error) {
	if len(m.Body) > MaxBody {
		return Event{}, invalid("body of %d bytes is over %d", len(m.Body), MaxBody)
	}
	if m.ContentType != "" {
		media, _, err := mime.ParseMediaType(m.ContentType)
		if err != nil || media != "application/json" {
			return Event{}, invalid("content-type is not application/json")
		}
	}
	var body keycloakBody
	decoder := json.NewDecoder(bytes.NewReader(m.Body))
	if err := decoder.Decode(&body); err != nil {
		return Event{}, invalid("body is not a Keycloak event")
	}
	if decoder.More() {
		return Event{}, invalid("body has more than one JSON value")
	}
	kc := KeycloakEvent{
		RealmID: text(body.RealmID), ClientID: text(body.ClientID), UserID: text(body.UserID),
		Failed: text(body.Error) != "",
	}
	for _, field := range []struct{ name, value string }{{"realmId", kc.RealmID}, {"clientId", kc.ClientID}, {"userId", kc.UserID}} {
		if len(field.value) > maxAttribute {
			return Event{}, invalid("%s is longer than %d bytes", field.name, maxAttribute)
		}
	}
	event := Event{Source: KeycloakSource, Subject: kc.UserID}
	if text(body.OperationType) != "" || text(body.ResourceType) != "" {
		kc.Admin = true
		kc.ResourceType, kc.OperationType = text(body.ResourceType), text(body.OperationType)
		if !keycloakToken.MatchString(kc.ResourceType) || !keycloakToken.MatchString(kc.OperationType) {
			return Event{}, invalid("an admin event's resourceType or operationType is not a Keycloak name")
		}
		event.Type = "keycloak.admin." + strings.ToLower(kc.ResourceType) + "." + strings.ToLower(kc.OperationType)
	} else {
		kc.Type = text(body.Type)
		if !keycloakToken.MatchString(kc.Type) {
			return Event{}, invalid("type is missing or not a Keycloak name")
		}
		event.Type = "keycloak." + strings.ToLower(kc.Type)
	}
	switch id := text(body.ID); {
	case id != "":
		event.ID = id
	case m.MessageID != "":
		event.ID = m.MessageID
	default:
		sum := sha256.Sum256(m.Body)
		event.ID = "sha256:" + hex.EncodeToString(sum[:])
	}
	if len(event.ID) > maxAttribute {
		return Event{}, invalid("id is longer than %d bytes", maxAttribute)
	}
	if ms, err := strconv.ParseInt(string(bytes.TrimSpace(body.Time)), 10, 64); err == nil && ms > 0 {
		event.Time = time.UnixMilli(ms).UTC()
	}
	data, err := json.Marshal(kc)
	if err != nil {
		return Event{}, invalid("event cannot be kept")
	}
	event.Data = data
	return event, nil
}

// KeycloakOf reads back what DecodeKeycloak kept of an event.
func KeycloakOf(e Event) (KeycloakEvent, error) {
	var kc KeycloakEvent
	if e.Source != KeycloakSource {
		return kc, errors.New("events: not a Keycloak event")
	}
	if err := json.Unmarshal(e.Data, &kc); err != nil {
		return kc, errors.New("events: not a Keycloak event")
	}
	return kc, nil
}

// text is a JSON string field, empty when it is absent or null.
func text(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

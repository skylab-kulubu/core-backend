package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"strconv"
	"strings"
	"time"
)

// MaxBody is the largest message core reads (ADR-0071: a publisher refuses
// a larger one). A larger delivery is not an event core can trust.
const MaxBody = 64 << 10

// maxAttribute bounds an id or a source: they are a primary key's columns.
const maxAttribute = 255

// CloudEventsContentType is the AMQP content-type of a structured-mode
// CloudEvent.
const CloudEventsContentType = "application/cloudevents+json"

// Message is a delivery as a Decoder sees it: its body and the AMQP
// properties a decoder may read.
type Message struct {
	Body        []byte
	ContentType string
	MessageID   string
	Type        string
	AppID       string
	Headers     map[string]any
	// RoutingKey is the key it was published with.
	RoutingKey string
}

// Event is one Integration event, decoded. Source and ID together name it
// (CloudEvents: a consumer may treat a repeat of the pair as a duplicate).
type Event struct {
	ID      string
	Source  string
	Type    string
	Subject string
	// DataSchema is the schema URN; its last part is the schema version
	// (urn:skylab:schema:forms.response.created:1).
	DataSchema string
	// Time is when it happened; zero when the publisher sent none.
	Time time.Time
	// Data is the event's data, as JSON.
	Data json.RawMessage
	// Queue it arrived on, and whether the broker delivered it before.
	Queue       string
	Redelivered bool
}

// SchemaVersion is the number after DataSchema's last colon, false when
// there is none.
func (e Event) SchemaVersion() (int, bool) {
	i := strings.LastIndexByte(e.DataSchema, ':')
	if i < 0 {
		return 0, false
	}
	v, err := strconv.Atoi(e.DataSchema[i+1:])
	if err != nil || v < 1 {
		return 0, false
	}
	return v, true
}

// Decoder turns a delivery into an Event. An error is an envelope core cannot
// trust: the delivery is rejected without requeue, to the queue's DLQ.
type Decoder func(Message) (Event, error)

// ErrInvalidEnvelope is every decoding failure.
var ErrInvalidEnvelope = errors.New("events: invalid envelope")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidEnvelope, fmt.Sprintf(format, args...))
}

// cloudEvent is the structured-mode JSON. data_base64 is read only to refuse
// it: core's events carry JSON data.
type cloudEvent struct {
	SpecVersion     string          `json:"specversion"`
	ID              string          `json:"id"`
	Source          string          `json:"source"`
	Type            string          `json:"type"`
	Subject         string          `json:"subject"`
	Time            string          `json:"time"`
	DataContentType string          `json:"datacontenttype"`
	DataSchema      string          `json:"dataschema"`
	Data            json.RawMessage `json:"data"`
	DataBase64      json.RawMessage `json:"data_base64"`
}

// DecodeCloudEvent reads a CloudEvents 1.0 event in structured JSON mode
// (ADR-0071 §5): specversion 1.0 and a non-empty id, source and type are
// required, time when present is RFC 3339, the data is JSON. Attributes it
// does not know are left alone. The AMQP content-type, when set, must be
// application/cloudevents+json.
func DecodeCloudEvent(m Message) (Event, error) {
	if len(m.Body) > MaxBody {
		return Event{}, invalid("body of %d bytes is over %d", len(m.Body), MaxBody)
	}
	if m.ContentType != "" {
		media, _, err := mime.ParseMediaType(m.ContentType)
		if err != nil || media != CloudEventsContentType {
			return Event{}, invalid("content-type is not %s", CloudEventsContentType)
		}
	}
	var ce cloudEvent
	decoder := json.NewDecoder(bytes.NewReader(m.Body))
	if err := decoder.Decode(&ce); err != nil {
		return Event{}, invalid("body is not a JSON object")
	}
	if decoder.More() {
		return Event{}, invalid("body has more than one JSON value")
	}
	if ce.SpecVersion != "1.0" {
		return Event{}, invalid("specversion is not 1.0")
	}
	for _, attr := range []struct{ name, value string }{{"id", ce.ID}, {"source", ce.Source}, {"type", ce.Type}} {
		if attr.value == "" {
			return Event{}, invalid("%s is missing", attr.name)
		}
		if len(attr.value) > maxAttribute {
			return Event{}, invalid("%s is longer than %d bytes", attr.name, maxAttribute)
		}
	}
	if len(ce.DataBase64) > 0 {
		return Event{}, invalid("data_base64 is not supported")
	}
	if ce.DataContentType != "" {
		media, _, err := mime.ParseMediaType(ce.DataContentType)
		if err != nil || media != "application/json" {
			return Event{}, invalid("datacontenttype is not application/json")
		}
	}
	event := Event{
		ID: ce.ID, Source: ce.Source, Type: ce.Type, Subject: ce.Subject,
		DataSchema: ce.DataSchema, Data: ce.Data,
	}
	if ce.Time != "" {
		at, err := time.Parse(time.RFC3339Nano, ce.Time)
		if err != nil {
			return Event{}, invalid("time is not RFC 3339")
		}
		event.Time = at.UTC()
	}
	if len(bytes.TrimSpace(event.Data)) == 0 || bytes.Equal(bytes.TrimSpace(event.Data), []byte("null")) {
		event.Data = nil
	}
	return event, nil
}

package events

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const validEvent = `{
  "specversion": "1.0",
  "id": "0192f0c4-1111-7000-8000-000000000001",
  "source": "/forms",
  "type": "forms.response.created",
  "time": "2026-10-20T09:15:00Z",
  "datacontenttype": "application/json",
  "dataschema": "urn:skylab:schema:forms.response.created:1",
  "subject": "form-1",
  "comexampleextension": "ignored",
  "data": {"formId": "form-1", "responseId": "r-1", "version": 3}
}`

func TestDecodeCloudEvent(t *testing.T) {
	event, err := DecodeCloudEvent(Message{Body: []byte(validEvent), ContentType: CloudEventsContentType})
	if err != nil {
		t.Fatal(err)
	}
	if event.ID != "0192f0c4-1111-7000-8000-000000000001" || event.Source != "/forms" || event.Type != "forms.response.created" ||
		event.Subject != "form-1" || !event.Time.Equal(time.Date(2026, 10, 20, 9, 15, 0, 0, time.UTC)) {
		t.Fatalf("event = %+v", event)
	}
	if v, ok := event.SchemaVersion(); !ok || v != 1 {
		t.Fatalf("schema version = %d %v", v, ok)
	}
	if !strings.Contains(string(event.Data), `"responseId": "r-1"`) {
		t.Fatalf("data = %s", event.Data)
	}
	// No content-type, a charset parameter, no time, no data: all fine.
	for _, m := range []Message{
		{Body: []byte(validEvent)},
		{Body: []byte(validEvent), ContentType: "application/cloudevents+json; charset=utf-8"},
		{Body: []byte(`{"specversion":"1.0","id":"1","source":"/s","type":"t"}`)},
		{Body: []byte(`{"specversion":"1.0","id":"1","source":"/s","type":"t","data":null}`)},
	} {
		event, err := DecodeCloudEvent(m)
		if err != nil {
			t.Fatalf("%s: %v", m.Body, err)
		}
		if strings.HasPrefix(string(m.Body), `{"specversion":"1.0","id":"1"`) && (event.Data != nil || !event.Time.IsZero()) {
			t.Fatalf("empty data or time read as %+v", event)
		}
	}
}

func TestDecodeCloudEventRefusesWhatItCannotTrust(t *testing.T) {
	long := strings.Repeat("x", maxAttribute+1)
	for name, m := range map[string]Message{
		"not json":         {Body: []byte("forms.response.created")},
		"two values":       {Body: []byte(validEvent + validEvent)},
		"array":            {Body: []byte(`[1]`)},
		"old spec":         {Body: []byte(`{"specversion":"0.3","id":"1","source":"/s","type":"t"}`)},
		"no id":            {Body: []byte(`{"specversion":"1.0","source":"/s","type":"t"}`)},
		"no source":        {Body: []byte(`{"specversion":"1.0","id":"1","type":"t"}`)},
		"no type":          {Body: []byte(`{"specversion":"1.0","id":"1","source":"/s"}`)},
		"long id":          {Body: []byte(`{"specversion":"1.0","id":"` + long + `","source":"/s","type":"t"}`)},
		"bad time":         {Body: []byte(`{"specversion":"1.0","id":"1","source":"/s","type":"t","time":"yesterday"}`)},
		"binary data":      {Body: []byte(`{"specversion":"1.0","id":"1","source":"/s","type":"t","data_base64":"AAAA"}`)},
		"xml data":         {Body: []byte(`{"specversion":"1.0","id":"1","source":"/s","type":"t","datacontenttype":"application/xml"}`)},
		"amqp content":     {Body: []byte(validEvent), ContentType: "application/json"},
		"bad content-type": {Body: []byte(validEvent), ContentType: ";;"},
		"too large":        {Body: []byte(`{"specversion":"1.0","id":"1","source":"/s","type":"t","data":"` + strings.Repeat("a", MaxBody) + `"}`)},
		"id not a string":  {Body: []byte(`{"specversion":"1.0","id":1,"source":"/s","type":"t"}`)},
	} {
		if _, err := DecodeCloudEvent(m); !errors.Is(err, ErrInvalidEnvelope) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestSchemaVersion(t *testing.T) {
	for schema, want := range map[string]int{
		"urn:skylab:schema:forms.form.updated:2": 2,
		"urn:skylab:schema:forms.form.updated":   0,
		"":                                       0,
		"urn:x:0":                                0,
	} {
		v, ok := Event{DataSchema: schema}.SchemaVersion()
		if v != want || ok != (want > 0) {
			t.Fatalf("%q: %d %v", schema, v, ok)
		}
	}
}

// traceparent (CloudEvents Distributed Tracing, W3C Trace Context) is read
// when it is valid and left empty otherwise: the extension is optional and a
// bad value never refuses the event.
func TestDecodeCloudEventTraceParent(t *testing.T) {
	const valid = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	with := func(value string) Message {
		return Message{Body: []byte(`{"specversion":"1.0","id":"1","source":"/forms","type":"t","traceparent":` + value + `}`)}
	}
	event, err := DecodeCloudEvent(with(`"` + valid + `"`))
	if err != nil {
		t.Fatal(err)
	}
	if event.TraceParent != valid || event.TraceID() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("traceparent %q, trace id %q", event.TraceParent, event.TraceID())
	}
	for name, value := range map[string]string{
		"upper case":      `"00-4BF92F3577B34DA6A3CE929D0E0E4736-00F067AA0BA902B7-01"`,
		"short trace id":  `"00-4bf92f3577b34da6a3ce929d0e0e473-00f067aa0ba902b7-01"`,
		"long parent id":  `"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b70-01"`,
		"zero trace id":   `"00-00000000000000000000000000000000-00f067aa0ba902b7-01"`,
		"zero parent id":  `"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01"`,
		"version ff":      `"ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"`,
		"other version":   `"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"`,
		"trailing data":   `"` + valid + `-x"`,
		"surrounding ws":  `" ` + valid + `"`,
		"empty":           `""`,
		"not a string":    `42`,
		"an object":       `{"traceparent":"` + valid + `"}`,
		"null":            `null`,
		"request id only": `"0HN7P2D3K4L5M"`,
	} {
		event, err := DecodeCloudEvent(with(value))
		if err != nil {
			t.Fatalf("%s: refused: %v", name, err)
		}
		if event.TraceParent != "" || event.TraceID() != "" {
			t.Fatalf("%s: traceparent %q, trace id %q", name, event.TraceParent, event.TraceID())
		}
	}
	// An event without the extension decodes as before.
	event, err = DecodeCloudEvent(Message{Body: []byte(validEvent), ContentType: CloudEventsContentType})
	if err != nil || event.TraceParent != "" || event.TraceID() != "" {
		t.Fatalf("event %+v, err %v", event, err)
	}
}

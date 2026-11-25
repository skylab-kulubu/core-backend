package event

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
)

// The detail fields an Event's Ticket holders may be told of when the
// editor says so (event.changed, docs/event-updated-notification.md#other-details).
const (
	FieldName         = "name"
	FieldDescription  = "description"
	FieldAttendeeNote = "attendeeNote"
	FieldCapacity     = "capacity"
	FieldLinks        = "links"
)

// DetailFields are the detail fields in the order a mail names them.
var DetailFields = []string{FieldName, FieldDescription, FieldAttendeeNote, FieldCapacity, FieldLinks}

// DetailDigests are the SHA-256 digests of e's detail fields: what a queued
// change keeps of their values before it (never the text), and what the
// worker compares the Event of the moment with. The links are the external
// registration link, LinkedIn, the application form and the extra forms
// (label and address); a short link's alias is not a change for the holder.
func DetailDigests(e Event) map[string]string {
	note := ""
	if e.AttendeeNote != nil {
		note = *e.AttendeeNote
	}
	type link struct{ Label, URL string }
	extra := make([]link, 0, len(e.ExtraFormURLs))
	for _, l := range e.ExtraFormURLs {
		extra = append(extra, link{l.Label, l.URL})
	}
	links, _ := json.Marshal(struct {
		External, Linkedin, Form string
		Extra                    []link
	}{e.ExternalRegistrationURL, e.Linkedin, e.FormURL, extra})
	values := map[string]string{
		FieldName:         e.Name,
		FieldDescription:  e.Description,
		FieldAttendeeNote: note,
		FieldCapacity:     strconv.Itoa(e.Capacity),
		FieldLinks:        string(links),
	}
	out := make(map[string]string, len(values))
	for field, value := range values {
		sum := sha256.Sum256([]byte(value))
		out[field] = hex.EncodeToString(sum[:])
	}
	return out
}

// changedDetails are the detail fields whose digests differ, in
// DetailFields' order.
func changedDetails(before, after map[string]string) []string {
	var out []string
	for _, field := range DetailFields {
		if before[field] != after[field] {
			out = append(out, field)
		}
	}
	return out
}

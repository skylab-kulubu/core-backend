package formsfeed

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestDecodeResponse(t *testing.T) {
	r, err := DecodeResponse(json.RawMessage(`{"formId":"` + applicationForm.String() + `","responseId":"` + responseID.String() + `",
		"status":"pending","userId":null,"hasGuest":true,"submittedAt":"2026-11-20 09:15","hiddenFields":{"davet":"ok","bad":"a@b.c","long":"` + string(make([]byte, 0)) + `"},"version":12}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.SubmittedAt != nil || !r.SubmittedAtIgnored || r.Version != 12 || r.Invite() != "ok" || len(r.HiddenFields) != 1 {
		t.Fatalf("%+v", r)
	}
	for _, bad := range []string{
		`{}`,
		`{"formId":"` + applicationForm.String() + `","responseId":"` + responseID.String() + `","status":"accepted"}`,
		`{"formId":"` + applicationForm.String() + `","responseId":"` + responseID.String() + `","status":"accepted","version":-1}`,
		`{"formId":"` + applicationForm.String() + `","responseId":"` + responseID.String() + `","status":"accepted","version":"yesterday"}`,
		`{"formId":"` + applicationForm.String() + `","responseId":"` + responseID.String() + `","status":"accepted","version":1,"userId":"00000000-0000-0000-0000-000000000000"}`,
		`null`,
	} {
		if _, err := DecodeResponse(json.RawMessage(bad)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v", bad, err)
		}
	}
}

func TestDecodeFormState(t *testing.T) {
	st, err := DecodeFormState(json.RawMessage(`{"formId":"` + feedbackForm.String() + `","status":"open","closesAt":"2026-12-01T00:00:00+03:00","version":"2026-11-24T10:00:00Z"}`))
	if err != nil || st.ClosesAt == nil || !st.ClosesAt.Equal(time.Date(2026, 11, 30, 21, 0, 0, 0, time.UTC)) || st.Version == 0 {
		t.Fatalf("%+v %v", st, err)
	}
	if _, err := DecodeFormState(json.RawMessage(`{"formId":"` + feedbackForm.String() + `","status":"open","closesAt":"soon","version":1}`)); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

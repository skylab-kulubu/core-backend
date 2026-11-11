package ticket_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/feedback"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

type fakeFeedbackLinks struct {
	links map[uuid.UUID][]feedback.TicketLink
	err   error
	calls [][]uuid.UUID
}

func (f *fakeFeedbackLinks) TicketLinks(_ context.Context, ids []uuid.UUID, _ time.Time) (map[uuid.UUID][]feedback.TicketLink, error) {
	f.calls = append(f.calls, ids)
	return f.links, f.err
}

func mineWithLinks(t *testing.T, links *fakeFeedbackLinks, extras ...any) (authz.Principal, []ticket.Ticket, ticket.Service) {
	t.Helper()
	events := event.NewMemoryStore()
	svc := ticket.NewService(ticket.NewMemoryStore(), events, authz.NewAuthorizer(authz.DefaultPolicy()), append([]any{links}, extras...)...)
	p := authz.Principal{ID: uuid.NewString()}
	var created []ticket.Ticket
	for range 2 {
		tk, err := svc.Apply(context.Background(), p, seedEvent(t, events, "WEBLAB").ID)
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, tk)
	}
	return p, created, svc
}

// The holder's own tickets carry each Ticket's feedback links, read once for
// all of them; a Ticket with none has no feedback field at all.
func TestService_MineCarriesFeedbackLinks(t *testing.T) {
	t.Parallel()
	links := &fakeFeedbackLinks{}
	p, created, svc := mineWithLinks(t, links)
	session := uuid.New()
	links.links = map[uuid.UUID][]feedback.TicketLink{created[0].ID: {
		{URL: "https://forms.example.test/f1?davet=AAAAAAAAAAAAAAAAAAAAAA", Title: "Açılış", SessionID: &session},
		{URL: "https://forms.example.test/f2?davet=BBBBBBBBBBBBBBBBBBBBBB", Title: "Hack", Answered: true},
	}}
	mine, err := svc.Mine(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(links.calls) != 1 || len(links.calls[0]) != 2 {
		t.Fatalf("calls %v", links.calls)
	}
	byID := map[uuid.UUID]ticket.Ticket{}
	for _, tk := range mine {
		byID[tk.ID] = tk
	}
	with, without := byID[created[0].ID], byID[created[1].ID]
	if len(with.Feedback) != 2 || with.Feedback[0].SessionID == nil || *with.Feedback[0].SessionID != session || !with.Feedback[1].Answered {
		t.Fatalf("with %+v", with.Feedback)
	}
	if without.Feedback != nil {
		t.Fatalf("without %+v", without.Feedback)
	}
	raw, err := json.Marshal(with)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"feedback":[{"url":"https://forms.example.test/f1?davet=AAAAAAAAAAAAAAAAAAAAAA","title":"Açılış","sessionId":"`+session.String()+`","answered":false},{"url":"https://forms.example.test/f2?davet=BBBBBBBBBBBBBBBBBBBBBB","title":"Hack","sessionId":null,"answered":true}]`) {
		t.Fatalf("json %s", raw)
	}
	raw, err = json.Marshal(without)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"feedback"`) {
		t.Fatalf("json %s", raw)
	}
}

// The links are an extra: when they cannot be read the Tickets still come,
// and a log line says so, naming nobody.
func TestService_MineWithoutFeedbackLinksOnError(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	p, created, svc := mineWithLinks(t, &fakeFeedbackLinks{err: errors.New("down")}, log.New(&logged, "", 0))
	mine, err := svc.Mine(context.Background(), p)
	if err != nil || len(mine) != len(created) {
		t.Fatalf("mine %+v %v", mine, err)
	}
	if line := logged.String(); !strings.Contains(line, "feedback links not read") || !strings.Contains(line, "down") || strings.Contains(line, p.ID) {
		t.Fatalf("log %q", line)
	}
	for _, tk := range mine {
		if tk.Feedback != nil {
			t.Fatalf("feedback %+v", tk.Feedback)
		}
	}
}

// Only the holder's own list carries them: a single Ticket read does not
// ask.
func TestService_GetHasNoFeedbackLinks(t *testing.T) {
	t.Parallel()
	links := &fakeFeedbackLinks{}
	p, created, svc := mineWithLinks(t, links)
	got, err := svc.Get(context.Background(), p, created[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Feedback != nil || len(links.calls) != 0 {
		t.Fatalf("get %+v, calls %v", got.Feedback, links.calls)
	}
}

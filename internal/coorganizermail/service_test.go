package coorganizermail_test

import (
	"context"
	"errors"
	"io"
	"log"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/coorganizermail"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type queue struct {
	rows      []coorganizermail.Queued
	completed []uuid.UUID
	retried   []uuid.UUID
}

func (q *queue) Claim(_ context.Context, now time.Time, _ time.Duration) (coorganizermail.Queued, bool, error) {
	if len(q.rows) == 0 {
		return coorganizermail.Queued{}, false, nil
	}
	row := q.rows[0]
	q.rows = q.rows[1:]
	row.ClaimedAt = now
	return row, true, nil
}

func (q *queue) Complete(_ context.Context, c coorganizermail.Queued) error {
	q.completed = append(q.completed, c.ID)
	return nil
}

func (q *queue) Retry(_ context.Context, c coorganizermail.Queued, _ time.Time) error {
	q.retried = append(q.retried, c.ID)
	return nil
}

type events map[uuid.UUID]event.Event

func (e events) GetIncludingArchived(_ context.Context, id uuid.UUID) (event.Event, error) {
	ev, ok := e[id]
	if !ok {
		return event.Event{}, event.ErrNotFound
	}
	return ev, nil
}

type users map[uuid.UUID]user.User

func (u users) Get(_ context.Context, id uuid.UUID) (user.User, error) {
	got, ok := u[id]
	if !ok {
		return user.User{}, user.ErrNotFound
	}
	return got, nil
}

type leaders map[string][]uuid.UUID

func (l leaders) LeaderIDs(_ context.Context, code string) ([]uuid.UUID, error) { return l[code], nil }

type sentMail struct {
	typ, key string
	subs     []uuid.UUID
	vars     map[string]string
}

type mailer struct {
	sent []sentMail
	err  error
}

func (m *mailer) CoOrganizer(_ context.Context, typ, key string, subs []uuid.UUID, vars map[string]string, _ mail.NotificationSource) error {
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, sentMail{typ, key, subs, vars})
	return nil
}

var now = time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)

// A person added is told, as is a team's Leaders (theirs alone, as the
// directory reads them); a change taken back meanwhile tells nobody; a
// team's mail carries its choices, a removal none; every variable is there.
func TestCoOrganizerMailIsSentToWhomTheChangeHolds(t *testing.T) {
	t.Parallel()
	end := now.Add(10 * 24 * time.Hour)
	person, gone, actor, lead1, lead2 := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	ev := event.Event{
		ID: uuid.New(), Name: "Hack", OwnerTeam: "YK", Status: event.StatusDraft, EndDate: &end,
		CoOrganizers:   []event.CoOrganizerTeam{{Code: "WEBLAB", Management: event.ManagementMembers, Visibility: event.VisibilityHelper}},
		CoOrganizerIDs: []uuid.UUID{person},
	}
	row := func(kind, team string, who uuid.UUID) coorganizermail.Queued {
		return coorganizermail.Queued{ID: uuid.New(), EventID: ev.ID, Kind: kind, TeamCode: team, UserID: who, ActorID: actor, CreatedAt: now.Add(-3 * time.Minute)}
	}
	addedPerson := row(event.CoOrganizerMailAdded, "", person)
	addedTeam := row(event.CoOrganizerMailAdded, "WEBLAB", uuid.Nil)
	removedPerson := row(event.CoOrganizerMailRemoved, "", gone)
	takenBack := row(event.CoOrganizerMailRemoved, "WEBLAB", uuid.Nil)
	noLeaders := row(event.CoOrganizerMailRemoved, "ARTLAB", uuid.Nil)
	q := &queue{rows: []coorganizermail.Queued{addedPerson, addedTeam, removedPerson, takenBack, noLeaders}}
	m := &mailer{}
	svc, err := coorganizermail.NewService(coorganizermail.Config{
		Queue: q, Events: events{ev.ID: ev}, Mail: m, PanelURL: "https://panel.example/",
		Users: users{
			person: {AccountState: user.AccountActive}, gone: {AccountState: user.AccountActive},
			actor: {AccountState: user.AccountActive, FirstName: "Yusuf", LastName: "Y"},
		},
		Leaders: leaders{"WEBLAB": {lead1, lead2}},
		Now:     func() time.Time { return now },
		Logger:  log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := svc.Pass(context.Background())
	if err != nil || report != (coorganizermail.Report{Sent: 3, Dropped: 2}) {
		t.Fatalf("report %+v %v", report, err)
	}
	if len(m.sent) != 3 || len(q.completed) != 5 {
		t.Fatalf("sent %+v completed %d", m.sent, len(q.completed))
	}
	toPerson, toTeam, toGone := m.sent[0], m.sent[1], m.sent[2]
	if toPerson.typ != mail.CoOrganizerAddedType || !slices.Equal(toPerson.subs, []uuid.UUID{person}) ||
		toPerson.key != "core:"+mail.CoOrganizerAddedType+":"+addedPerson.ID.String()+":v1" ||
		toPerson.vars["TeamName"] != "" || toPerson.vars["ActorName"] != "Yusuf Y" || toPerson.vars["PanelUrl"] != "https://panel.example/events/"+ev.ID.String() ||
		toPerson.vars["EventName"] != "Hack" || toPerson.vars["OwnerTeam"] == "" {
		t.Fatalf("person %+v", toPerson)
	}
	if !slices.Equal(toTeam.subs, []uuid.UUID{lead1, lead2}) || toTeam.vars["TeamName"] == "" ||
		toTeam.vars["Management"] != "bütün üyeler" || toTeam.vars["Visibility"] != "yardımcı ekip (dışarıda görünmez)" {
		t.Fatalf("team %+v", toTeam)
	}
	if toGone.typ != mail.CoOrganizerRemovedType || toGone.vars["Management"] != "" || len(toGone.vars) != 8 {
		t.Fatalf("removal %+v", toGone)
	}
}

// A person whose rights have ended, an archived Event, an inactive account
// and an old row tell nobody; SkyNotify's refusal is done with, its outage
// tried again.
func TestCoOrganizerMailDropsAndRetries(t *testing.T) {
	t.Parallel()
	ended := now.Add(-31 * 24 * time.Hour)
	person, blocked := uuid.New(), uuid.New()
	past := event.Event{ID: uuid.New(), Name: "Old", EndDate: &ended, CoOrganizerIDs: []uuid.UUID{person}}
	archivedAt := now
	archived := event.Event{ID: uuid.New(), Name: "Gone", ArchivedAt: &archivedAt, CoOrganizerIDs: []uuid.UUID{person}}
	current := event.Event{ID: uuid.New(), Name: "Now", CoOrganizerIDs: []uuid.UUID{person, blocked}}
	row := func(ev event.Event, who uuid.UUID, age time.Duration) coorganizermail.Queued {
		return coorganizermail.Queued{ID: uuid.New(), EventID: ev.ID, Kind: event.CoOrganizerMailAdded, UserID: who, CreatedAt: now.Add(-age)}
	}
	q := &queue{rows: []coorganizermail.Queued{
		row(past, person, time.Hour), row(archived, person, time.Hour), row(current, blocked, time.Hour), row(current, person, 8*24*time.Hour),
	}}
	m := &mailer{}
	config := coorganizermail.Config{
		Queue: q, Events: events{past.ID: past, archived.ID: archived, current.ID: current}, Mail: m,
		Users:   users{person: {AccountState: user.AccountActive}, blocked: {AccountState: user.AccountAnonymized}},
		Leaders: leaders{}, Now: func() time.Time { return now }, Logger: log.New(io.Discard, "", 0),
	}
	svc, err := coorganizermail.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	if report, err := svc.Pass(context.Background()); err != nil || report != (coorganizermail.Report{Dropped: 3, Expired: 1}) || len(m.sent) != 0 {
		t.Fatalf("report %+v %v %+v", report, err, m.sent)
	}

	q.rows = []coorganizermail.Queued{row(current, person, time.Hour)}
	m.err = &mail.SendError{Status: 422}
	if report, _ := svc.Pass(context.Background()); report.Rejected != 1 {
		t.Fatalf("refused %+v", report)
	}
	m.err = errors.New("down")
	q.rows = []coorganizermail.Queued{row(current, person, time.Hour)}
	if report, _ := svc.Pass(context.Background()); report.Retried != 1 || len(q.retried) != 1 {
		t.Fatalf("retried %+v", report)
	}
	if _, err := coorganizermail.NewService(coorganizermail.Config{Queue: q, Events: events{}, Mail: m, Users: users{}, Leaders: leaders{}, PanelURL: "http://panel.example"}); err == nil {
		t.Fatal("an http panel was taken")
	}
}

// DirectoryLeaders are the enabled people of the team's own Leader groups,
// once each: not its members, not a Team above's Leaders.
func TestDirectoryLeaders(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := identity.NewMemory()
	lead, coord, member, above, disabled := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, g := range []identity.Group{
		{ID: "g1", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"},
		{ID: "g2", Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/LIDERLER"},
		{ID: "g3", Name: "KOORDINATORLER", Path: "/UYELER/ARGE/WEBLAB/KOORDINATORLER"},
		{ID: "g4", Name: "LIDERLER", Path: "/UYELER/ARGE/LIDERLER"},
	} {
		dir.PutGroup(g)
	}
	for id, enabled := range map[uuid.UUID]bool{lead: true, coord: true, member: true, above: true, disabled: false} {
		dir.PutUser(identity.Person{ID: id, Enabled: enabled})
	}
	if err := dir.DisableUser(ctx, disabled); err != nil {
		t.Fatal(err)
	}
	for group, people := range map[string][]uuid.UUID{"g1": {member}, "g2": {lead, disabled}, "g3": {coord, lead}, "g4": {above}} {
		for _, id := range people {
			if err := dir.AddMember(ctx, group, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	d := coorganizermail.DirectoryLeaders{Teams: tree{"WEBLAB": "/UYELER/ARGE/WEBLAB"}, Directory: dir, LeaderSubgroups: []string{"LIDERLER", "KOORDINATORLER"}}
	got, err := d.LeaderIDs(ctx, "WEBLAB")
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(got, func(a, b uuid.UUID) int { return compare(a.String(), b.String()) })
	want := []uuid.UUID{lead, coord}
	slices.SortFunc(want, func(a, b uuid.UUID) int { return compare(a.String(), b.String()) })
	if !slices.Equal(got, want) {
		t.Fatalf("leaders %v, want %v", got, want)
	}
	if got, err := d.LeaderIDs(ctx, "NOLAB"); err != nil || got != nil {
		t.Fatalf("unknown team %v %v", got, err)
	}
}

type tree map[string]string

func (t tree) TeamPath(team string) (string, bool) { p, ok := t[team]; return p, ok }
func (tree) SubTeams(string) []string              { return nil }

func compare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

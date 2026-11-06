package competitor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/competitor"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

func setup(t *testing.T) (event.Store, competitor.Service) {
	t.Helper()
	events := event.NewMemoryStore()
	svc := competitor.NewService(competitor.NewMemoryStore(events), events, authz.NewAuthorizer(authz.DefaultPolicy()))
	return events, svc
}

func seedEvent(t *testing.T, events event.Store, owner string, status event.Status) event.Event {
	t.Helper()
	ev, err := events.Create(context.Background(), event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: owner, Status: status})
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestService_SelfRegisterThenListMine(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB", event.StatusPublished)
	userID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	score := 40.0
	p := authz.Principal{ID: userID.String()}

	created, err := svc.Create(ctx, p, competitor.CreateInput{UserID: userID, EventID: ev.ID, Score: &score})
	if err != nil {
		t.Fatal(err)
	}
	if created.UserID != userID || created.Score != nil {
		t.Fatalf("created %+v", created)
	}

	mine, err := svc.Mine(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].ID != created.ID {
		t.Fatalf("mine %+v", mine)
	}

	_, err = svc.Create(ctx, p, competitor.CreateInput{UserID: userID, EventID: ev.ID})
	if !errors.Is(err, competitor.ErrConflict) {
		t.Fatalf("dup: %v", err)
	}
}

func TestService_StaffRegisterOtherAndScore(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB", event.StatusPublished)
	userID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	score := 12.5
	staff := authz.Principal{ID: "staff", Groups: []string{"/UYELER/ARGE/WEBLAB"}}

	created, err := svc.Create(ctx, staff, competitor.CreateInput{UserID: userID, EventID: ev.ID, Score: &score, IsWinner: true})
	if err != nil {
		t.Fatal(err)
	}
	if created.Score == nil || *created.Score != 12.5 || !created.IsWinner {
		t.Fatalf("created %+v", created)
	}

	updated, err := svc.Update(ctx, staff, created.ID, competitor.UpdateInput{UserID: userID, EventID: ev.ID, Score: ptr(20), IsWinner: false})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Score == nil || *updated.Score != 20 || updated.IsWinner {
		t.Fatalf("updated %+v", updated)
	}
}

func TestService_StrangerCannotRegisterOther(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ev := seedEvent(t, events, "WEBLAB", event.StatusPublished)
	other := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	p := authz.Principal{ID: "skysec", Groups: []string{"/UYELER/ARGE/SKYSEC"}}
	_, err := svc.Create(context.Background(), p, competitor.CreateInput{UserID: other, EventID: ev.ID})
	if !errors.Is(err, competitor.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}

// A competitor is added to, and reinstated on, a published Event, an ended
// one too (results come after the Event, ADR 0066); never a draft (404 to
// who may not see it) or a cancelled Event.
func TestService_CompetitorsFollowTheEventStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := []struct {
		name   string
		status event.Status
		ended  bool
		reason error // nil: allowed
	}{
		{"published", event.StatusPublished, false, nil},
		{"published and ended", event.StatusPublished, true, nil},
		{"draft", event.StatusDraft, false, event.ErrNotPublished},
		{"cancelled", event.StatusCancelled, false, event.ErrCancelled},
	}
	for _, tc := range cases {
		events := event.NewMemoryStore()
		competitors := competitor.NewMemoryStore(events)
		svc := competitor.NewService(competitors, events, authz.NewAuthorizer(authz.DefaultPolicy()))
		in := event.Event{Name: tc.name, Location: "YTÜ", OwnerTeam: "WEBLAB", Status: tc.status}
		if tc.ended {
			start, end := time.Now().Add(-48*time.Hour), time.Now().Add(-40*time.Hour)
			in.StartDate, in.EndDate = &start, &end
		}
		ev, err := events.Create(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		userID := uuid.New()
		p := authz.Principal{ID: userID.String()}
		// One registered and withdrawn earlier, written to the store as it
		// was then.
		withdrawn, err := competitors.Create(ctx, competitor.Competitor{UserID: userID, EventID: ev.ID})
		if err != nil {
			t.Fatal(err)
		}
		if err := competitors.Withdraw(ctx, withdrawn.ID, &userID); err != nil {
			t.Fatal(err)
		}

		other := uuid.New()
		if tc.status == event.StatusDraft {
			// A draft does not exist for who may not see it (ADR 0066);
			// its editor is refused for its status.
			if _, err := svc.Create(ctx, authz.Principal{ID: other.String()}, competitor.CreateInput{UserID: other, EventID: ev.ID}); !errors.Is(err, competitor.ErrNotFound) {
				t.Fatalf("draft: create by a member: %v", err)
			}
			if _, err := svc.Reinstate(ctx, p, withdrawn.ID); !errors.Is(err, competitor.ErrNotFound) {
				t.Fatalf("draft: reinstate by a member: %v", err)
			}
			editor := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
			_, err := svc.Create(ctx, editor, competitor.CreateInput{UserID: other, EventID: ev.ID})
			if !errors.Is(err, competitor.ErrInvalid) || !errors.Is(err, tc.reason) {
				t.Fatalf("draft: create by its editor: %v", err)
			}
			if _, err := svc.Reinstate(ctx, editor, withdrawn.ID); !errors.Is(err, competitor.ErrConflict) || !errors.Is(err, tc.reason) {
				t.Fatalf("draft: reinstate by its editor: %v", err)
			}
			continue
		}
		_, err = svc.Create(ctx, authz.Principal{ID: other.String()}, competitor.CreateInput{UserID: other, EventID: ev.ID})
		if tc.reason == nil && err != nil {
			t.Fatalf("%s: create: %v", tc.name, err)
		}
		if tc.reason != nil && (!errors.Is(err, competitor.ErrInvalid) || !errors.Is(err, tc.reason)) {
			t.Fatalf("%s: create: %v", tc.name, err)
		}
		_, err = svc.Reinstate(ctx, p, withdrawn.ID)
		if tc.reason == nil && err != nil {
			t.Fatalf("%s: reinstate: %v", tc.name, err)
		}
		if tc.reason != nil && (!errors.Is(err, competitor.ErrConflict) || !errors.Is(err, tc.reason)) {
			t.Fatalf("%s: reinstate: %v", tc.name, err)
		}
	}
}

// A person who enters themselves is never the winner on entry, as they
// give themselves no score: before, anyone signed in could enter a
// published Event as its winner.
func TestService_SelfRegisterIsNoWinner(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB", event.StatusPublished)
	userID := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	created, err := svc.Create(ctx, authz.Principal{ID: userID.String()}, competitor.CreateInput{UserID: userID, EventID: ev.ID, IsWinner: true})
	if err != nil {
		t.Fatal(err)
	}
	if created.IsWinner || created.Score != nil {
		t.Fatalf("created %+v, want no score and no win", created)
	}
	// The Event's Leader still enters someone else as its winner.
	leader := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	other := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	won, err := svc.Create(ctx, leader, competitor.CreateInput{UserID: other, EventID: ev.ID, IsWinner: true})
	if err != nil {
		t.Fatal(err)
	}
	if !won.IsWinner {
		t.Fatalf("the Leader's winner %+v", won)
	}
}

func TestService_SelfCannotUpdateScore(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB", event.StatusPublished)
	userID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	p := authz.Principal{ID: userID.String()}
	created, err := svc.Create(ctx, p, competitor.CreateInput{UserID: userID, EventID: ev.ID})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Update(ctx, p, created.ID, competitor.UpdateInput{UserID: userID, EventID: ev.ID, Score: ptr(99)})
	if !errors.Is(err, competitor.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}

func TestService_SelfCanDelete(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB", event.StatusPublished)
	userID := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	p := authz.Principal{ID: userID.String()}
	created, err := svc.Create(ctx, p, competitor.CreateInput{UserID: userID, EventID: ev.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, p, created.ID); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Get(ctx, p, created.ID)
	if !errors.Is(err, competitor.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestService_WithdrawAndReinstateAreIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := event.NewMemoryStore()
	competitors := competitor.NewMemoryStore(events)
	svc := competitor.NewService(competitors, events, authz.NewAuthorizer(authz.DefaultPolicy()))
	ev := seedEvent(t, events, "WEBLAB", event.StatusPublished)
	userID := uuid.MustParse("99999999-9999-9999-9999-999999999999")
	p := authz.Principal{ID: userID.String()}
	created, err := svc.Create(ctx, p, competitor.CreateInput{UserID: userID, EventID: ev.ID})
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.Delete(ctx, p, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, p, created.ID); err != nil {
		t.Fatalf("second withdraw: %v", err)
	}
	withdrawn, err := svc.ListLifecycle(ctx, p, lifecycle.InactiveOnly)
	if err != nil || len(withdrawn) != 1 || withdrawn[0].WithdrawnBy == nil || *withdrawn[0].WithdrawnBy != userID {
		t.Fatalf("withdrawn list = %+v, err = %v", withdrawn, err)
	}

	reinstated, err := svc.Reinstate(ctx, p, created.ID)
	if err != nil || reinstated.WithdrawnAt != nil {
		t.Fatalf("reinstate = %+v, err = %v", reinstated, err)
	}
	if _, err := svc.Reinstate(ctx, p, created.ID); err != nil {
		t.Fatalf("second reinstate: %v", err)
	}

	if err := svc.Delete(ctx, p, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := events.Archive(ctx, ev.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reinstate(ctx, p, created.ID); !errors.Is(err, competitor.ErrConflict) {
		t.Fatalf("reinstate under archived event = %v", err)
	}
}

func TestService_DefaultListsHideCompetitorsUnderArchivedEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := event.NewMemoryStore()
	competitors := competitor.NewMemoryStore(events)
	svc := competitor.NewService(competitors, events, authz.NewAuthorizer(authz.DefaultPolicy()))
	ev := seedEvent(t, events, "WEBLAB", event.StatusPublished)
	userID := uuid.MustParse("99999999-9999-9999-9999-999999999999")
	user := authz.Principal{ID: userID.String()}
	created, err := svc.Create(ctx, user, competitor.CreateInput{UserID: userID, EventID: ev.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Archive(ctx, ev.ID, nil); err != nil {
		t.Fatal(err)
	}

	manager := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	listed, err := svc.List(ctx, manager)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("default list exposed archived parent: %+v", listed)
	}
	mine, err := svc.Mine(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 0 {
		t.Fatalf("mine exposed archived parent: %+v", mine)
	}

	archivedParent, err := svc.ListLifecycle(ctx, manager, lifecycle.All)
	if err != nil {
		t.Fatal(err)
	}
	if len(archivedParent) != 1 || archivedParent[0].ID != created.ID || archivedParent[0].Event == nil || archivedParent[0].Event.ID != ev.ID {
		t.Fatalf("lifecycle list lost archived parent context: %+v", archivedParent)
	}
}

func TestService_LeaderboardByOwnerTeamAndSeason(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	seasonA := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	seasonB := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	web := seedEvent(t, events, "WEBLAB", event.StatusPublished)
	web.SeasonID = &seasonA
	if _, err := events.Update(ctx, web); err != nil {
		t.Fatal(err)
	}
	web2 := seedEvent(t, events, "WEBLAB", event.StatusPublished)
	web2.SeasonID = &seasonB
	if _, err := events.Update(ctx, web2); err != nil {
		t.Fatal(err)
	}
	sky := seedEvent(t, events, "SKYSEC", event.StatusPublished)

	ada := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	bob := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	staff := authz.Principal{ID: "staff", Groups: []string{"/UYELER/YK"}}

	mustCreate := func(user uuid.UUID, ev uuid.UUID, score float64) {
		t.Helper()
		if _, err := svc.Create(ctx, staff, competitor.CreateInput{UserID: user, EventID: ev, Score: &score}); err != nil {
			t.Fatal(err)
		}
	}
	mustCreate(ada, web.ID, 10)
	mustCreate(ada, web2.ID, 5)
	mustCreate(bob, web.ID, 10)
	mustCreate(ada, sky.ID, 100)

	board, err := svc.Leaderboard(ctx, staff, "WEBLAB", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(board) != 2 {
		t.Fatalf("team board %+v", board)
	}
	if board[0].TotalScore != 15 || board[0].UserID != ada || board[0].Rank != 1 || board[0].EventCount != 2 {
		t.Fatalf("ada %+v", board[0])
	}
	if board[1].UserID != bob || board[1].TotalScore != 10 || board[1].Rank != 2 {
		t.Fatalf("bob %+v", board[1])
	}

	seasonBoard, err := svc.Leaderboard(ctx, staff, "WEBLAB", &seasonA)
	if err != nil {
		t.Fatal(err)
	}
	if len(seasonBoard) != 2 {
		t.Fatalf("season board %+v", seasonBoard)
	}
	if seasonBoard[0].TotalScore != 10 || seasonBoard[0].Rank != 1 || seasonBoard[1].Rank != 1 {
		t.Fatalf("tied ranks %+v", seasonBoard)
	}

	winner, err := svc.Winner(ctx, staff, web.ID)
	if !errors.Is(err, competitor.ErrNotFound) {
		t.Fatalf("no winner yet: %+v %v", winner, err)
	}
	comps, err := svc.ListByEvent(ctx, staff, web.ID)
	if err != nil || len(comps) != 2 {
		t.Fatalf("event comps %+v %v", comps, err)
	}
	if _, err := svc.Update(ctx, staff, comps[0].ID, competitor.UpdateInput{UserID: comps[0].UserID, EventID: web.ID, Score: comps[0].Score, IsWinner: true}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Winner(ctx, staff, web.ID)
	if err != nil || got.ID != comps[0].ID {
		t.Fatalf("winner %+v %v", got, err)
	}
}

func ptr(v float64) *float64 {
	return &v
}

// keptToLeaders answers MembersManageCompetitors: false for the Teams whose
// Leader kept the competitors to the Leaders.
type keptToLeaders map[string]bool

func (k keptToLeaders) MembersManageCompetitors(team string) bool { return !k[team] }

func (k keptToLeaders) TeamCompetitionManage(string) bool { return false }

// Moving a competitor to another Event takes it out of the Event it is in:
// who moves it writes the competitors of both. A writer of team B does not
// move team A's competitor to B's Event, with A's setting on or off; A's
// Leader and Privileged do. An edit that keeps the Event is decided as
// before, on that Event alone.
func TestService_MovingACompetitorTakesTheWriteOfBothEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, aKeptToLeaders := range []bool{false, true} {
		events := event.NewMemoryStore()
		az := authz.NewAuthorizer(authz.DefaultPolicy(), authz.WithTeamSettings(keptToLeaders{"WEBLAB": aKeptToLeaders}))
		svc := competitor.NewService(competitor.NewMemoryStore(events), events, az)
		a := seedEvent(t, events, "WEBLAB", event.StatusPublished)
		a2 := seedEvent(t, events, "WEBLAB", event.StatusPublished)
		b := seedEvent(t, events, "SKYSEC", event.StatusPublished)
		yk := authz.Principal{ID: "yk", Groups: []string{"/UYELER/YK"}}
		aLeader := authz.Principal{ID: "a-leader", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
		bWriter := authz.Principal{ID: "b-writer", Groups: []string{"/UYELER/ARGE/SKYSEC"}}

		inA, err := svc.Create(ctx, yk, competitor.CreateInput{UserID: uuid.New(), EventID: a.ID})
		if err != nil {
			t.Fatal(err)
		}
		_, err = svc.Update(ctx, bWriter, inA.ID, competitor.UpdateInput{EventID: b.ID, Score: ptr(1)})
		if !errors.Is(err, competitor.ErrForbidden) {
			t.Fatalf("A kept to Leaders %v: B's writer moved A's competitor to B's Event: %v", aKeptToLeaders, err)
		}
		if got, err := svc.Get(ctx, yk, inA.ID); err != nil || got.EventID != a.ID || got.Score != nil {
			t.Fatalf("A's competitor after the refused move %+v %v", got, err)
		}

		moved, err := svc.Update(ctx, aLeader, inA.ID, competitor.UpdateInput{EventID: a2.ID})
		if err != nil || moved.EventID != a2.ID {
			t.Fatalf("A's Leader moving within A %+v %v", moved, err)
		}
		moved, err = svc.Update(ctx, yk, inA.ID, competitor.UpdateInput{EventID: b.ID})
		if err != nil || moved.EventID != b.ID {
			t.Fatalf("Privileged moving A's competitor to B %+v %v", moved, err)
		}

		// Kept in its Event, B's writer edits B's competitor as before.
		scored, err := svc.Update(ctx, bWriter, inA.ID, competitor.UpdateInput{Score: ptr(7), IsWinner: true})
		if err != nil || scored.EventID != b.ID || scored.Score == nil || *scored.Score != 7 || !scored.IsWinner {
			t.Fatalf("B's writer editing in place %+v %v", scored, err)
		}
		scored, err = svc.Update(ctx, bWriter, inA.ID, competitor.UpdateInput{EventID: b.ID, Score: ptr(8)})
		if err != nil || scored.Score == nil || *scored.Score != 8 {
			t.Fatalf("B's writer naming the same Event %+v %v", scored, err)
		}
	}
}

// A member of both Teams writes B's competitors but not A's while A's Leader
// keeps them to the Leaders: they do not move A's competitor to B's Event.
// With A's setting on, they write both and move it.
func TestService_AMemberOfBothTeamsMovesOnlyWhatTheSourceTeamLets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, aKeptToLeaders := range []bool{false, true} {
		events := event.NewMemoryStore()
		competitors := competitor.NewMemoryStore(events)
		az := authz.NewAuthorizer(authz.DefaultPolicy(), authz.WithTeamSettings(keptToLeaders{"WEBLAB": aKeptToLeaders}))
		svc := competitor.NewService(competitors, events, az)
		a := seedEvent(t, events, "WEBLAB", event.StatusPublished)
		b := seedEvent(t, events, "SKYSEC", event.StatusPublished)
		bothMember := authz.Principal{ID: "both-member", Groups: []string{"/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/SKYSEC"}}

		inA, err := competitors.Create(ctx, competitor.Competitor{UserID: uuid.New(), EventID: a.ID})
		if err != nil {
			t.Fatal(err)
		}
		moved, err := svc.Update(ctx, bothMember, inA.ID, competitor.UpdateInput{EventID: b.ID, Score: ptr(1)})
		if aKeptToLeaders {
			if !errors.Is(err, competitor.ErrForbidden) {
				t.Fatalf("A kept to Leaders: a member of both moved A's competitor to B: %+v %v", moved, err)
			}
			if got, err := competitors.Get(ctx, inA.ID); err != nil || got.EventID != a.ID || got.Score != nil {
				t.Fatalf("A's competitor after the refused move %+v %v", got, err)
			}
			continue
		}
		if err != nil || moved.EventID != b.ID || moved.Score == nil || *moved.Score != 1 {
			t.Fatalf("A open to members: a member of both moving A's competitor to B %+v %v", moved, err)
		}
	}
}

// The Event a competitor is moved out of is read as Delete reads it: a draft
// its mover may not see does not exist for them (404); a draft they see but
// whose competitors they do not write refuses them (403). An archived Event
// still asks for its write: its Leader moves the competitor out, B's writer
// does not.
func TestService_MovingOutOfADraftOrArchivedEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := event.NewMemoryStore()
	competitors := competitor.NewMemoryStore(events)
	az := authz.NewAuthorizer(authz.DefaultPolicy(), authz.WithTeamSettings(keptToLeaders{"GECEKODU": true}))
	svc := competitor.NewService(competitors, events, az)
	b := seedEvent(t, events, "SKYSEC", event.StatusPublished)
	bWriter := authz.Principal{ID: "b-writer", Groups: []string{"/UYELER/ARGE/SKYSEC"}}

	unchanged := func(t *testing.T, id, eventID uuid.UUID) {
		t.Helper()
		if got, err := competitors.Get(ctx, id); err != nil || got.EventID != eventID || got.Score != nil {
			t.Fatalf("competitor after the refused move %+v %v", got, err)
		}
	}

	// A WEBLAB draft: B's writer does not see it.
	draft := seedEvent(t, events, "WEBLAB", event.StatusDraft)
	inDraft, err := competitors.Create(ctx, competitor.Competitor{UserID: uuid.New(), EventID: draft.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(ctx, bWriter, inDraft.ID, competitor.UpdateInput{EventID: b.ID, Score: ptr(1)}); !errors.Is(err, competitor.ErrNotFound) {
		t.Fatalf("moving out of a draft its mover does not see: %v", err)
	}
	unchanged(t, inDraft.ID, draft.ID)

	// A GECEKODU draft: its members edit GECEKODU's Events and see it, but
	// its Leader kept the competitors to the Leaders.
	seenDraft := seedEvent(t, events, "GECEKODU", event.StatusDraft)
	inSeenDraft, err := competitors.Create(ctx, competitor.Competitor{UserID: uuid.New(), EventID: seenDraft.ID})
	if err != nil {
		t.Fatal(err)
	}
	draftReader := authz.Principal{ID: "draft-reader", Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU", "/UYELER/ARGE/SKYSEC"}}
	if !event.VisibleTo(az, &draftReader, seenDraft) {
		t.Fatal("the GECEKODU member does not see GECEKODU's draft")
	}
	if _, err := svc.Update(ctx, draftReader, inSeenDraft.ID, competitor.UpdateInput{EventID: b.ID, Score: ptr(1)}); !errors.Is(err, competitor.ErrForbidden) {
		t.Fatalf("moving out of a draft its mover sees but does not write: %v", err)
	}
	unchanged(t, inSeenDraft.ID, seenDraft.ID)

	// An archived WEBLAB Event.
	archived := seedEvent(t, events, "WEBLAB", event.StatusPublished)
	a2 := seedEvent(t, events, "WEBLAB", event.StatusPublished)
	inArchived, err := competitors.Create(ctx, competitor.Competitor{UserID: uuid.New(), EventID: archived.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Archive(ctx, archived.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(ctx, bWriter, inArchived.ID, competitor.UpdateInput{EventID: b.ID, Score: ptr(1)}); !errors.Is(err, competitor.ErrForbidden) {
		t.Fatalf("B's writer moving out of an archived Event: %v", err)
	}
	unchanged(t, inArchived.ID, archived.ID)
	aLeader := authz.Principal{ID: "a-leader", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	moved, err := svc.Update(ctx, aLeader, inArchived.ID, competitor.UpdateInput{EventID: a2.ID})
	if err != nil || moved.EventID != a2.ID {
		t.Fatalf("A's Leader moving out of an archived Event %+v %v", moved, err)
	}
}

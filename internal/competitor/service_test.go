package competitor_test

import (
	"context"
	"errors"
	"testing"

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

func seedEvent(t *testing.T, events event.Store, owner string, active bool) event.Event {
	t.Helper()
	ev, err := events.Create(context.Background(), event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: owner, Active: active})
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestService_SelfRegisterThenListMine(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB", true)
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
	ev := seedEvent(t, events, "WEBLAB", true)
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
	ev := seedEvent(t, events, "WEBLAB", true)
	other := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	p := authz.Principal{ID: "skysec", Groups: []string{"/UYELER/ARGE/SKYSEC"}}
	_, err := svc.Create(context.Background(), p, competitor.CreateInput{UserID: other, EventID: ev.ID})
	if !errors.Is(err, competitor.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}

func TestService_InactiveEventRejected(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ev := seedEvent(t, events, "WEBLAB", false)
	userID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	_, err := svc.Create(context.Background(), authz.Principal{ID: userID.String()}, competitor.CreateInput{UserID: userID, EventID: ev.ID})
	if !errors.Is(err, competitor.ErrInvalid) {
		t.Fatalf("got %v", err)
	}
}

func TestService_SelfCannotUpdateScore(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB", true)
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
	ev := seedEvent(t, events, "WEBLAB", true)
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
	ev := seedEvent(t, events, "WEBLAB", true)
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
	ev := seedEvent(t, events, "WEBLAB", true)
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
	web := seedEvent(t, events, "WEBLAB", true)
	web.SeasonID = &seasonA
	if _, err := events.Update(ctx, web); err != nil {
		t.Fatal(err)
	}
	web2 := seedEvent(t, events, "WEBLAB", true)
	web2.SeasonID = &seasonB
	if _, err := events.Update(ctx, web2); err != nil {
		t.Fatal(err)
	}
	sky := seedEvent(t, events, "SKYSEC", true)

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

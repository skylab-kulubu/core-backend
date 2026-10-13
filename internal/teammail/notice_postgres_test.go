package teammail_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/teammail"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestPostgresNoticeQueue(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	q := teammail.NewPostgresNoticeQueue(pool)
	now := time.Date(2026, 10, 12, 19, 0, 0, 0, time.UTC)
	actor, subject := uuid.New(), uuid.New()
	change := teammail.Notice{
		ID: uuid.New(), ChangeID: uuid.New(), Event: teammail.NoticeRolesChanged, GroupPath: "/UYELER",
		ActorID: &actor, RolesAdded: []string{"core:users:manage"}, OccurredAt: now,
	}
	member := teammail.Notice{
		ID: uuid.New(), ChangeID: uuid.New(), Event: teammail.NoticeMemberAdded, GroupPath: "/UYELER/YK",
		SubjectID: &subject, OccurredAt: now.Add(time.Second),
	}
	for _, n := range []teammail.Notice{change, member} {
		if err := q.Enqueue(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := q.Claim(ctx, now, 2*time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].ID != change.ID || claimed[0].RecipientID != nil || claimed[0].Event != teammail.NoticeRolesChanged ||
		!slices.Equal(claimed[0].RolesAdded, []string{"core:users:manage"}) || len(claimed[0].RolesRemoved) != 0 || *claimed[0].ActorID != actor {
		t.Fatalf("claimed %+v", claimed)
	}
	// A claim another core has taken since expands nothing.
	stale := claimed[0]
	stale.ClaimedAt = now.Add(-time.Hour)
	if n, err := q.Expand(ctx, stale, []uuid.UUID{uuid.New()}, nil, now); err != nil || n != 0 {
		t.Fatalf("stale expand %d %v", n, err)
	}
	a, b := uuid.New(), uuid.New()
	count := 302
	if n, err := q.Expand(ctx, claimed[0], []uuid.UUID{a, b, a}, &count, now); err != nil || n != 2 {
		t.Fatalf("expand %d %v", n, err)
	}
	due, err := q.Claim(ctx, now.Add(time.Second), 2*time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	var recipients []uuid.UUID
	for _, n := range due {
		if n.ChangeID == change.ChangeID {
			if n.RecipientID == nil || n.MemberCount == nil || *n.MemberCount != 302 || n.GroupPath != "/UYELER" || n.ID == change.ID {
				t.Fatalf("expanded %+v", n)
			}
			recipients = append(recipients, *n.RecipientID)
		}
	}
	if len(due) != 3 || len(recipients) != 2 || !slices.Contains(recipients, a) || !slices.Contains(recipients, b) {
		t.Fatalf("due %+v", due)
	}
	for _, n := range due {
		if err := q.Retry(ctx, n, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if backlog, age, err := q.Backlog(ctx, now.Add(time.Minute)); err != nil || backlog != 3 || age != time.Minute {
		t.Fatalf("backlog %d %v %v", backlog, age, err)
	}
	again, err := q.Claim(ctx, now.Add(time.Minute), 2*time.Minute, 10)
	if err != nil || len(again) != 3 || again[0].Attempts != 1 {
		t.Fatalf("again %+v %v", again, err)
	}
	if err := q.Complete(ctx, again[0]); err != nil {
		t.Fatal(err)
	}
	if dropped, err := q.DropBefore(ctx, now.Add(time.Hour)); err != nil || dropped != 2 {
		t.Fatalf("dropped %d %v", dropped, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO privilege_notice_mails (id, change_id, event, group_path, occurred_at, next_attempt_at) VALUES ($1, $2, 'other', '/x', now(), now())`, uuid.New(), uuid.New()); err == nil {
		t.Fatal("unknown event accepted")
	}
}

func TestAccountErasureForgetsQueuedPrivilegeNotices(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	users := user.NewPostgresStore(pool)
	erased, other := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{erased, other} {
		if _, _, err := user.NewService(users).Ensure(ctx, id, user.Profile{Email: id.String() + "@example.test"}); err != nil {
			t.Fatal(err)
		}
	}
	q := teammail.NewPostgresNoticeQueue(pool)
	now := time.Now().UTC()
	about := teammail.Notice{ID: uuid.New(), ChangeID: uuid.New(), Event: teammail.NoticeMemberAdded, GroupPath: "/UYELER/YK", SubjectID: &erased, ActorID: &erased, OccurredAt: now}
	if err := q.Enqueue(ctx, about); err != nil {
		t.Fatal(err)
	}
	claimed, err := q.Claim(ctx, now, time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claimed %+v %v", claimed, err)
	}
	if _, err := q.Expand(ctx, claimed[0], []uuid.UUID{erased, other}, nil, now); err != nil {
		t.Fatal(err)
	}
	if _, err := users.RequestDeletion(ctx, erased, nil); err != nil {
		t.Fatal(err)
	}
	if err := users.AnonymizeAccount(ctx, erased, now, nil); err != nil {
		t.Fatal(err)
	}
	left, err := q.Claim(ctx, now, time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || *left[0].RecipientID != other || left[0].SubjectID != nil || left[0].ActorID != nil {
		t.Fatalf("after erasure %+v", left)
	}
}

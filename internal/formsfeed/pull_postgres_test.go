package formsfeed_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/formsfeed"
)

func TestPostgresPullsQueueAndPlan(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	q := formsfeed.NewPostgresPulls(pool)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	form, eventID, person := uuid.New(), uuid.New(), uuid.New()

	// A backfill a form at a time.
	if ok, err := q.Backfill(ctx, form, eventID, person, now); err != nil || !ok {
		t.Fatalf("backfill: %t %v", ok, err)
	}
	if ok, err := q.Backfill(ctx, form, eventID, person, now); err != nil || ok {
		t.Fatalf("second backfill: %t %v", ok, err)
	}
	job, ok, err := q.Claim(ctx, now)
	if err != nil || !ok || job.FormID != form || job.Kind != formsfeed.PullBackfill || job.EventID == nil || *job.EventID != eventID {
		t.Fatalf("claim: %+v %t %v", job, ok, err)
	}
	// Leased: not due again until the lease ends.
	if _, ok, _ := q.Claim(ctx, now.Add(time.Minute)); ok {
		t.Fatal("a leased job was claimed again")
	}
	if err := q.SaveCursor(ctx, job.ID, "p2"); err != nil {
		t.Fatal(err)
	}
	if err := q.Retry(ctx, job.ID, 1, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	job, ok, _ = q.Claim(ctx, now.Add(2*time.Minute))
	if !ok || job.Cursor != "p2" || job.Attempts != 1 {
		t.Fatalf("retried job %+v", job)
	}
	watermark := now.Add(-time.Hour)
	if err := q.Finish(ctx, job, &watermark, now); err != nil {
		t.Fatal(err)
	}
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM form_pulls`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d jobs left", n)
	}

	// The daily pass: a form seen the first time gets now as its
	// watermark and no job; a day later a catch-up job from watermark − 5
	// minutes, once.
	other := uuid.New()
	if queued, err := q.Plan(ctx, []uuid.UUID{form, other}, now); err != nil || queued != 0 {
		t.Fatalf("first plan: %d %v", queued, err)
	}
	var got time.Time
	pool.QueryRow(ctx, `SELECT watermark FROM form_pull_watermarks WHERE form_id = $1`, form).Scan(&got)
	if !got.Equal(watermark) {
		t.Fatalf("the backfill's watermark was replaced: %v", got)
	}
	later := now.Add(formsfeed.CatchupEvery + time.Minute)
	if queued, err := q.Plan(ctx, []uuid.UUID{form, other}, later); err != nil || queued != 2 {
		t.Fatalf("plan a day later: %d %v", queued, err)
	}
	if queued, _ := q.Plan(ctx, []uuid.UUID{form, other}, later); queued != 0 {
		t.Fatalf("planned twice: %d", queued)
	}
	var since time.Time
	pool.QueryRow(ctx, `SELECT changed_since FROM form_pulls WHERE form_id = $1 AND kind = 'catchup'`, other).Scan(&since)
	if !since.Equal(now.Add(-formsfeed.WatermarkMargin)) {
		t.Fatalf("changed_since %v", since)
	}
	// A watermark goes forward only.
	if err := q.Finish(ctx, formsfeed.PullJob{ID: uuid.New(), FormID: form}, &now, later); err != nil {
		t.Fatal(err)
	}
	older := now.Add(-48 * time.Hour)
	if err := q.Finish(ctx, formsfeed.PullJob{ID: uuid.New(), FormID: form}, &older, later); err != nil {
		t.Fatal(err)
	}
	pool.QueryRow(ctx, `SELECT watermark FROM form_pull_watermarks WHERE form_id = $1`, form).Scan(&got)
	if !got.Equal(now) {
		t.Fatalf("watermark went back: %v", got)
	}
	// A form no Event lists any more loses its watermark after 30 days.
	if _, err := q.Plan(ctx, []uuid.UUID{form}, later.Add(31*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	pool.QueryRow(ctx, `SELECT count(*) FROM form_pull_watermarks WHERE form_id = $1`, other).Scan(&n)
	if n != 0 {
		t.Fatal("an unlisted form's watermark stayed")
	}
}

package usersync_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
	"github.com/skylab-kulubu/core-backend/internal/usersync"
)

type pageDir []identity.Person

func (d pageDir) UsersPage(_ context.Context, first, max int) ([]identity.Person, error) {
	if first >= len(d) {
		return nil, nil
	}
	return d[first:min(first+max, len(d))], nil
}

type sentMail struct{ sent []user.User }

func (m *sentMail) Welcome(_ context.Context, u user.User) mail.WelcomeOutcome {
	m.sent = append(m.sent, u)
	return mail.WelcomeSent
}

// On PostgreSQL: the pass opens the missing row with a Sky number, fills a
// row's blanks without overwriting, and marks each mail it sends once.
func TestPostgresPass(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	accounts := user.NewService(store)
	since := time.Date(2026, 10, 18, 21, 0, 0, 0, time.UTC)
	created := since.Add(time.Hour)

	missing := identity.Person{ID: uuid.New(), Email: "new@example.com", FirstName: "Ada", LastName: "Lovelace", Username: "ada.lovelace", Enabled: true, CreatedAt: &created}
	blank := identity.Person{ID: uuid.New(), Email: "blank@example.com", FirstName: "Grace", LastName: "Hopper", Enabled: true, CreatedAt: &created}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, last_name, sky_number) VALUES ($1, 'Kept', 'SKY-0000900')`, blank.ID); err != nil {
		t.Fatal(err)
	}
	mailer := &sentMail{}
	w := usersync.New(pageDir{missing, blank}, accounts, usersync.NewPostgres(pool), mailer,
		usersync.Config{Enabled: true, Interval: time.Minute, WelcomeSince: since}, nil)
	for range 2 {
		if _, err := w.Pass(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.Get(ctx, missing.ID)
	if err != nil || got.SkyNumber == "" || got.Email != "new@example.com" || got.FirstName != "Ada" {
		t.Fatalf("opened row %+v %v", got, err)
	}
	got, err = store.Get(ctx, blank.ID)
	if err != nil || got.Email != "blank@example.com" || got.FirstName != "Grace" || got.LastName != "Kept" || got.SkyNumber != "SKY-0000900" {
		t.Fatalf("filled row %+v %v", got, err)
	}
	if len(mailer.sent) != 2 {
		t.Fatalf("sent %d mails, want 2 over two passes", len(mailer.sent))
	}
	var marked int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE welcome_sent_at IS NOT NULL AND id = ANY($1)`, []uuid.UUID{missing.ID, blank.ID}).Scan(&marked); err != nil || marked != 2 {
		t.Fatalf("marked %d %v", marked, err)
	}
}

// The mark: one claim wins, a release takes back only its own mark, and a
// row being erased cannot be claimed.
func TestPostgresWelcomeMark(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	rows := usersync.NewPostgres(pool)
	id, pending, noEmail := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, first_name, username, school_email, sky_number) VALUES
		($1, 'a@example.com', 'Ada', 'ada.lovelace', 'ada@std.yildiz.edu.tr', 'SKY-0000001'),
		($2, 'p@example.com', 'Pen', '', '', 'SKY-0000002'),
		($3, '', 'Nil', '', '', 'SKY-0000003')`, id, pending, noEmail); err != nil {
		t.Fatal(err)
	}
	if _, err := user.NewPostgresStore(pool).RequestDeletion(ctx, pending, nil); err != nil {
		t.Fatal(err)
	}

	u, at, ok, err := rows.ClaimWelcome(ctx, id)
	if err != nil || !ok || u.Email != "a@example.com" || u.Username != "ada.lovelace" || u.SchoolEmail != "ada@std.yildiz.edu.tr" || u.SkyNumber != "SKY-0000001" {
		t.Fatalf("claim %+v %v %v", u, ok, err)
	}
	if _, _, ok, err := rows.ClaimWelcome(ctx, id); ok || err != nil {
		t.Fatalf("second claim %v %v", ok, err)
	}
	if err := rows.ReleaseWelcome(ctx, id, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if states, err := rows.States(ctx, []uuid.UUID{id}); err != nil || !states[id].Welcomed {
		t.Fatalf("another mark's release must not clear it: %+v %v", states, err)
	}
	if err := rows.ReleaseWelcome(ctx, id, at); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := rows.ClaimWelcome(ctx, id); !ok || err != nil {
		t.Fatalf("claim after release %v %v", ok, err)
	}
	for _, other := range []uuid.UUID{pending, noEmail} {
		if _, _, ok, err := rows.ClaimWelcome(ctx, other); ok || err != nil {
			t.Fatalf("%s claimed: %v %v", other, ok, err)
		}
	}
	states, err := rows.States(ctx, []uuid.UUID{id, pending, noEmail, uuid.New()})
	if err != nil || len(states) != 3 || states[pending].Active || !states[noEmail].Active {
		t.Fatalf("states %+v %v", states, err)
	}
	if filled, err := rows.FillMissing(ctx, pending, "", "", "Late"); filled || err != nil {
		t.Fatalf("a row being erased was filled: %v %v", filled, err)
	}
	if filled, err := rows.FillMissing(ctx, id, "other@example.com", "Other", ""); filled || err != nil {
		t.Fatalf("nothing blank to fill: %v %v", filled, err)
	}
}

// One pass at a time across cores: the lock is the database's.
func TestPostgresLockIsExclusive(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	first, second := usersync.NewPostgres(pool), usersync.NewPostgres(pool)
	release, ok, err := first.Lock(ctx)
	if err != nil || !ok {
		t.Fatalf("first lock %v %v", ok, err)
	}
	if _, ok, err := second.Lock(ctx); ok || err != nil {
		t.Fatalf("second lock while held %v %v", ok, err)
	}
	release()
	release2, ok, err := second.Lock(ctx)
	if err != nil || !ok {
		t.Fatalf("second lock after release %v %v", ok, err)
	}
	release2()
}

package identity_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// rosterQueries counts the queries core sends to Postgres.
type rosterQueries struct{ n atomic.Int64 }

func (c *rosterQueries) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (c *rosterQueries) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// The public team list reads its members' profiles, pictures and erasure
// state in one query, however many members the team has, and leaves out
// everyone core may no longer show: being erased, anonymized, hard-purged
// (a deletion marker without a row), and the placeholder subject.
func TestPublicRosterReadsProfilesInOneQueryAndLeavesOutErasedPeopleOnPostgres(t *testing.T) {
	ctx := context.Background()
	plain := testpostgres.Start(t)
	if err := migrate.Apply(ctx, plain); err != nil {
		t.Fatal(err)
	}
	config := plain.Config()
	queries := &rosterQueries{}
	config.ConnConfig.Tracer = queries
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	store := user.NewPostgresStore(pool)
	dir := identity.NewMemory()
	svc := identity.NewService(dir, store, authz.NewAuthorizer(authz.DefaultPolicy()))
	dir.PutGroup(identity.Group{ID: "g-one", Name: "ONE", Path: "/UYELER/ARGE/ONE", Attributes: map[string]string{"public_listing": "true"}})
	dir.PutGroup(identity.Group{ID: "g-four", Name: "FOUR", Path: "/UYELER/ARGE/FOUR", Attributes: map[string]string{"public_listing": "true"}})

	member := func(group, first string, stored bool) uuid.UUID {
		t.Helper()
		id := uuid.New()
		person := identity.Person{ID: id, Email: id.String() + "@example.test", FirstName: first, LastName: "Keycloak"}
		dir.PutUser(person)
		if err := dir.AddMember(ctx, group, id); err != nil {
			t.Fatal(err)
		}
		if stored {
			if _, _, err := user.NewService(store).Ensure(ctx, id, user.Profile{Email: person.Email, FirstName: first, LastName: "Core"}); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	read := func(team string) (identity.Roster, int64) {
		t.Helper()
		queries.n.Store(0)
		roster, err := svc.PublicMembers(ctx, team)
		if err != nil {
			t.Fatal(err)
		}
		return roster, queries.n.Load()
	}

	member("g-one", "Solo", true)
	one, oneQueries := read("ONE")
	if one.Count != 1 || oneQueries != 1 {
		t.Fatalf("1 member took %d queries: %+v", oneQueries, one.Members)
	}

	ada := member("g-four", "Ada", true)
	member("g-four", "Grace", true)
	member("g-four", "Unstored", false)
	pending := member("g-four", "Pending", true)
	anonymized := member("g-four", "Anonymized", true)
	purged := member("g-four", "Purged", true)
	dir.PutUser(identity.Person{ID: user.DeletedSubject, FirstName: "Placeholder"})
	if err := dir.AddMember(ctx, "g-four", user.DeletedSubject); err != nil {
		t.Fatal(err)
	}
	picture, err := media.NewPostgresStore(pool).Create(ctx, media.Media{
		Name: "ada.jpg", Type: "image/jpeg", Kind: media.KindImage, Key: "images/ada", UploadedBy: ada,
		Purpose: media.PurposeProfilePicture, Width: 1600, Height: 1600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := user.NewService(store).SetProfilePicture(ctx, ada, picture.ID, picture.Key); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{pending, anonymized, purged} {
		if _, err := store.RequestDeletion(ctx, id, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []uuid.UUID{anonymized, purged} {
		if err := store.AnonymizeAccount(ctx, id, time.Now().UTC(), nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE account_deletion_requests SET status = 'completed', completed_at = now() WHERE subject_id = $1`, purged); err != nil {
		t.Fatal(err)
	}
	if err := store.HardPurgeAccount(ctx, purged); err != nil {
		t.Fatal(err)
	}

	four, fourQueries := read("FOUR")
	if fourQueries != oneQueries {
		t.Fatalf("listing 3 shown and 4 hidden members took %d queries, 1 member took %d", fourQueries, oneQueries)
	}
	got := map[string]identity.PublicMember{}
	for _, m := range four.Members {
		got[m.FirstName+" "+m.LastName] = m
	}
	if four.Count != 3 || len(got) != 3 {
		t.Fatalf("members %+v", four.Members)
	}
	for _, name := range []string{"Ada Core", "Grace Core", "Unstored Keycloak"} {
		if _, ok := got[name]; !ok {
			t.Fatalf("%s missing from %+v", name, four.Members)
		}
	}
	if a := got["Ada Core"]; a.ProfilePictureURL != media.PublicURL("", "images/ada") || len(a.ProfilePictureSizes) == 0 {
		t.Fatalf("ada %+v", a)
	}
	if g := got["Grace Core"]; g.ProfilePictureURL != "" || g.ProfilePictureSizes != nil {
		t.Fatalf("grace %+v", g)
	}
}

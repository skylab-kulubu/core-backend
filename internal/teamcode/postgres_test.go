package teamcode_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/teamcode"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func migrated(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func held(t *testing.T, ledger teamcode.Ledger, code string) bool {
	t.Helper()
	var got bool
	if err := ledger.Locked(context.Background(), func(ctx context.Context, records teamcode.Records) error {
		var err error
		got, err = records.Held(ctx, code)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return got
}

// Every record that names an Owner team holds its code, and so does a
// retired code, whatever the case. A binding of another scope does not.
func TestPostgresLedgerHeld(t *testing.T) {
	pool := migrated(t)
	ledger := teamcode.NewPostgresLedger(pool)
	event, ticket := uuid.New(), uuid.New()
	exec(t, pool, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'EVENTTEAM')`, event)
	exec(t, pool, `INSERT INTO tickets (id, event_id, ticket_type, guest_email) VALUES ($1, $2, 'GUEST', 'guest@example.test')`, ticket, event)
	exec(t, pool, `INSERT INTO certificates (id, event_id, ticket_id, serial, recipient_name, recipient_email, event_name, owner_team, verify_url)
		VALUES ($1, $2, $3, 'S-1', 'R', 'r@example.test', 'Hack', 'CertTeam', 'https://example.test/v')`, uuid.New(), event, ticket)
	template := uuid.New()
	exec(t, pool, `INSERT INTO certificate_templates (id, name, owner_team) VALUES ($1, 'T', 'TEMPLATETEAM')`, template)
	exec(t, pool, `INSERT INTO certificate_template_bindings (id, scope, scope_key, template_id) VALUES ($1, 'ownerTeam', 'BOUNDTEAM', $2)`, uuid.New(), template)
	exec(t, pool, `INSERT INTO certificate_template_bindings (id, scope, scope_key, template_id) VALUES ($1, 'event', 'EVENTSCOPE', $2)`, uuid.New(), template)
	exec(t, pool, `INSERT INTO retired_team_codes (code, reason) VALUES ('OLDTEAM', 'seeded')`)

	for _, code := range []string{"EVENTTEAM", "eventteam", "CERTTEAM", "TEMPLATETEAM", "BOUNDTEAM", "OLDTEAM", "oldteam"} {
		if !held(t, ledger, code) {
			t.Errorf("%s is not held", code)
		}
	}
	for _, code := range []string{"EVENTSCOPE", "WEBLAB", "EVENTTEA"} {
		if held(t, ledger, code) {
			t.Errorf("%s is held", code)
		}
	}
	// References names the kinds of record that hold a code, never a
	// record's id, and leaves retired codes out.
	exec(t, pool, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Jam', 'YTÜ', 'templateteam')`, uuid.New())
	for code, want := range map[string][]string{
		"EVENTTEAM":    {teamcode.RecordEvent},
		"certteam":     {teamcode.RecordCertificate},
		"TEMPLATETEAM": {teamcode.RecordEvent, teamcode.RecordCertificateTemplate},
		"BOUNDTEAM":    {teamcode.RecordTemplateBinding},
		"OLDTEAM":      nil,
		"EVENTSCOPE":   nil,
	} {
		if got := references(t, ledger, code); !slices.Equal(got, want) {
			t.Errorf("References(%s) = %v, want %v", code, got, want)
		}
	}
}

func references(t *testing.T, ledger teamcode.Ledger, code string) []string {
	t.Helper()
	var got []string
	if err := ledger.Locked(context.Background(), func(ctx context.Context, records teamcode.Records) error {
		var err error
		got, err = records.References(ctx, code)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return got
}

// A retired code is kept once the write succeeds, idempotently, with the
// Group's id when it is a UUID; a write that fails retires nothing.
func TestPostgresLedgerRetire(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	ledger := teamcode.NewPostgresLedger(pool)
	failed := errors.New("keycloak down")
	if err := ledger.Locked(ctx, func(ctx context.Context, records teamcode.Records) error {
		if err := records.Retire(ctx, "GAMELAB", uuid.NewString(), teamcode.ReasonRenamed); err != nil {
			return err
		}
		return failed
	}); !errors.Is(err, failed) {
		t.Fatalf("Locked = %v", err)
	}
	if held(t, ledger, "GAMELAB") {
		t.Fatal("a failed write retired its code")
	}

	group := uuid.New()
	for range 2 {
		if err := ledger.Locked(ctx, func(ctx context.Context, records teamcode.Records) error {
			return records.Retire(ctx, "GAMELAB", group.String(), teamcode.ReasonRenamed)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ledger.Locked(ctx, func(ctx context.Context, records teamcode.Records) error {
		return records.Retire(ctx, "AIRLAB", "g-not-a-uuid", teamcode.ReasonRenamed)
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT code, group_id, reason FROM retired_team_codes ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct {
		code   string
		group  *uuid.UUID
		reason string
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.code, &r.group, &r.reason); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) != 2 || got[0].code != "AIRLAB" || got[0].group != nil || got[1].code != "GAMELAB" ||
		got[1].group == nil || *got[1].group != group || got[1].reason != teamcode.ReasonRenamed {
		t.Fatalf("retired %+v", got)
	}
}

// slowCreates holds every Group create between its call and the write,
// long enough for a second create to check the name meanwhile.
type slowCreates struct {
	*identity.Memory
	delay time.Duration
}

func (d slowCreates) CreateGroup(ctx context.Context, parentRef, name string) (identity.Group, error) {
	time.Sleep(d.delay)
	return d.Memory.CreateGroup(ctx, parentRef, name)
}

// Two creates of one name at once, on two cores (two services over one
// database): the lock lets one check and write before the other checks, so
// exactly one Group is created and the other is refused as taken. Without
// the lock both checks pass before either write.
func TestPostgresLedgerLockSerializesCreates(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	dir := identity.NewMemory()
	dir.PutGroup(identity.Group{ID: "g-arge", Name: "ARGE", Path: "/UYELER/ARGE"})
	dir.PutGroup(identity.Group{ID: "g-orga", Name: "ORGANIZASYON", Path: "/UYELER/ORGANIZASYON"})
	slow := slowCreates{Memory: dir, delay: 300 * time.Millisecond}
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	yk := authz.Principal{ID: "yk", Groups: []string{"/UYELER/YK"}}
	service := func() identity.Service {
		return identity.NewServiceWithOptions(slow, user.NewMemoryStore(), az, identity.Options{TeamCodes: teamcode.NewPostgresLedger(pool)})
	}
	cores := []identity.Service{service(), service()}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, parent := range []string{"g-arge", "g-orga"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = cores[i].CreateGroup(ctx, yk, parent, "NEWLAB")
		}()
	}
	wg.Wait()
	var created, taken int
	for _, err := range errs {
		var refused *teamcode.Error
		switch {
		case err == nil:
			created++
		case errors.As(err, &refused) && refused.Code == teamcode.NameTaken:
			taken++
		default:
			t.Fatalf("CreateGroup: %v", err)
		}
	}
	if created != 1 || taken != 1 {
		t.Fatalf("created %d, refused as taken %d; want 1 and 1", created, taken)
	}
	groups, err := dir.ListGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, g := range groups {
		if g.Name == "NEWLAB" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d Groups named NEWLAB", n)
	}
}

// Through the service: a code an Owner team record or a retired code holds
// is refused, and a rename retires the old code in the database.
func TestServiceNamesWithPostgres(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	dir := identity.NewMemory()
	dir.PutGroup(identity.Group{ID: "g-arge", Name: "ARGE", Path: "/UYELER/ARGE"})
	weblab := uuid.NewString()
	dir.PutGroup(identity.Group{ID: weblab, Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"})
	exec(t, pool, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Eski', 'YTÜ', 'SKYSEC')`, uuid.New())
	exec(t, pool, `INSERT INTO retired_team_codes (code, reason) VALUES ('MOBILAB', 'seeded')`)
	svc := identity.NewServiceWithOptions(dir, user.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()),
		identity.Options{TeamCodes: teamcode.NewPostgresLedger(pool)})
	yk := authz.Principal{ID: "yk", Groups: []string{"/UYELER/YK"}}

	for _, name := range []string{"SKYSEC", "skysec", "MOBILAB"} {
		_, err := svc.CreateGroup(ctx, yk, "g-arge", name)
		var refused *teamcode.Error
		if !errors.As(err, &refused) || (refused.Code != teamcode.NameTaken && refused.Code != teamcode.InvalidName) {
			t.Errorf("create %s: %v", name, err)
		}
	}
	if _, err := svc.UpdateGroup(ctx, yk, weblab, "WEB", nil); err != nil {
		t.Fatal(err)
	}
	var reason string
	var group uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT reason, group_id FROM retired_team_codes WHERE code = 'WEBLAB'`).Scan(&reason, &group); err != nil {
		t.Fatal(err)
	}
	if reason != teamcode.ReasonRenamed || group.String() != weblab {
		t.Fatalf("retired WEBLAB: %s %s", reason, group)
	}
	// The old code is nobody's now.
	_, err := svc.CreateGroup(ctx, yk, "g-arge", "WEBLAB")
	var refused *teamcode.Error
	if !errors.As(err, &refused) || refused.Code != teamcode.NameTaken {
		t.Fatalf("create the retired WEBLAB: %v", err)
	}
}

// A database that gives no lock, or no check, is ErrUnavailable and the
// write is never run: no Group is written.
func TestPostgresLedgerUnavailable(t *testing.T) {
	pool := migrated(t)
	ledger := teamcode.NewPostgresLedger(pool)
	if _, err := pool.Exec(context.Background(), `ALTER TABLE retired_team_codes RENAME TO retired_team_codes_gone`); err != nil {
		t.Fatal(err)
	}
	err := ledger.Locked(context.Background(), func(ctx context.Context, records teamcode.Records) error {
		_, err := records.Held(ctx, "WEBLAB")
		return err
	})
	if !errors.Is(err, teamcode.ErrUnavailable) {
		t.Fatalf("check: %v", err)
	}
	pool.Close()
	ran := false
	err = ledger.Locked(context.Background(), func(context.Context, teamcode.Records) error {
		ran = true
		return nil
	})
	if !errors.Is(err, teamcode.ErrUnavailable) || ran {
		t.Fatalf("closed pool: %v, write ran %v", err, ran)
	}
}

// Through the service with the database (ADR 0065 ticket 05): a Team whose
// code an Event names keeps it (code_in_use, naming the kind), a Team whose
// Sub-team's code an Event names is not deleted, and a deletion retires the
// codes of the Team and of every Sub-team under it.
func TestTeamStructureWithPostgres(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	dir := identity.NewMemory()
	root := uuid.NewString()
	dir.PutGroup(identity.Group{ID: root, Name: "YK", Path: "/UYELER/YK"})
	if err := dir.SetGroupClientRoles(ctx, root, []identity.ClientRole{{ClientID: "core", Role: authz.RoleGroupsManage}}); err != nil {
		t.Fatal(err)
	}
	dir.PutGroup(identity.Group{ID: uuid.NewString(), Name: "UYELER", Path: "/UYELER"})
	dir.PutGroup(identity.Group{ID: uuid.NewString(), Name: "ARGE", Path: "/UYELER/ARGE"})
	weblab, front, inner := uuid.NewString(), uuid.NewString(), uuid.NewString()
	dir.PutGroup(identity.Group{ID: weblab, Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"})
	dir.PutGroup(identity.Group{ID: front, Name: "FRONT", Path: "/UYELER/ARGE/WEBLAB/FRONT"})
	dir.PutGroup(identity.Group{ID: inner, Name: "INNER", Path: "/UYELER/ARGE/WEBLAB/FRONT/INNER"})
	exec(t, pool, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'inner')`, uuid.New())
	// A Team goes with its Sub-teams only with Team delegation on.
	policy := authz.DefaultPolicy()
	policy.TeamDelegation = true
	svc := identity.NewServiceWithOptions(dir, user.NewMemoryStore(), authz.NewAuthorizer(policy),
		identity.Options{TeamCodes: teamcode.NewPostgresLedger(pool), Delegation: identity.NewTeamDelegation(policy, dir, dir, identity.TeamDelegationOptions{})})
	yk := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}

	_, err := svc.ChangeTeamCode(ctx, yk, inner, "INNER2")
	var structure *identity.TeamStructureError
	if !errors.As(err, &structure) || structure.Code != identity.CodeCodeInUse || !slices.Equal(structure.Records, []string{teamcode.RecordEvent}) {
		t.Fatalf("change a code an Event names: %v", err)
	}
	err = svc.DeleteGroup(ctx, yk, front)
	var deletion *identity.GroupDeleteError
	if !errors.As(err, &deletion) || deletion.Code != identity.CodeCodeInUse || !slices.Equal(deletion.Records, []string{teamcode.RecordEvent}) {
		t.Fatalf("delete a Team whose Sub-team's code an Event names: %v", err)
	}
	exec(t, pool, `DELETE FROM events WHERE owner_team = 'inner'`)
	if err := svc.DeleteGroup(ctx, yk, front); err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"FRONT", "INNER"} {
		var reason string
		if err := pool.QueryRow(ctx, `SELECT reason FROM retired_team_codes WHERE code = $1`, code).Scan(&reason); err != nil || reason != teamcode.ReasonDeleted {
			t.Errorf("%s: %q %v", code, reason, err)
		}
	}
	if _, err := dir.GetGroup(ctx, inner); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("INNER remains: %v", err)
	}
}

// The team structure report reads the codes core's records name, by kind,
// blanks trimmed and blank ones left out, and the retired codes; seeding
// retires the codes that are not retired yet, whatever the case, once.
func TestPostgresLedgerSeedRetired(t *testing.T) {
	pool := migrated(t)
	ledger := teamcode.NewPostgresLedger(pool)
	ctx := context.Background()
	event, ticket := uuid.New(), uuid.New()
	exec(t, pool, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', ' GONE ')`, event)
	exec(t, pool, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Jam', 'YTÜ', '  ')`, uuid.New())
	exec(t, pool, `INSERT INTO tickets (id, event_id, ticket_type, guest_email) VALUES ($1, $2, 'GUEST', 'guest@example.test')`, ticket, event)
	exec(t, pool, `INSERT INTO certificates (id, event_id, ticket_id, serial, recipient_name, recipient_email, event_name, owner_team, verify_url)
		VALUES ($1, $2, $3, 'S-1', 'R', 'r@example.test', 'Hack', 'GONE', 'https://example.test/v')`, uuid.New(), event, ticket)
	template := uuid.New()
	exec(t, pool, `INSERT INTO certificate_templates (id, name, owner_team) VALUES ($1, 'T', 'WEBLAB')`, template)
	exec(t, pool, `INSERT INTO certificate_template_bindings (id, scope, scope_key, template_id) VALUES ($1, 'ownerTeam', 'OLD', $2)`, uuid.New(), template)
	exec(t, pool, `INSERT INTO certificate_template_bindings (id, scope, scope_key, template_id) VALUES ($1, 'event', 'EVENTSCOPE', $2)`, uuid.New(), template)
	exec(t, pool, `INSERT INTO retired_team_codes (code, reason) VALUES ('Old', 'renamed')`)

	records, err := ledger.RecordCodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"GONE":   {teamcode.RecordEvent, teamcode.RecordCertificate},
		"WEBLAB": {teamcode.RecordCertificateTemplate},
		"OLD":    {teamcode.RecordTemplateBinding},
	}
	if len(records) != len(want) {
		t.Fatalf("record codes = %v, want %v", records, want)
	}
	for code, kinds := range want {
		if !slices.Equal(records[code], kinds) {
			t.Errorf("record codes[%s] = %v, want %v", code, records[code], kinds)
		}
	}

	calls := 0
	codes := func(context.Context) ([]string, error) {
		calls++
		return []string{"GONE", "OLD", "cmsteam", "CMSTEAM"}, nil
	}
	written, err := ledger.SeedRetired(ctx, codes)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(written, []string{"GONE", "cmsteam"}) {
		t.Fatalf("written = %v, want [GONE cmsteam] (OLD is retired as Old; CMSTEAM is cmsteam)", written)
	}
	again, err := ledger.SeedRetired(ctx, codes)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 || calls != 2 {
		t.Fatalf("second run wrote %v (calls %d)", again, calls)
	}
	retired, err := ledger.RetiredCodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(retired, []string{"GONE", "Old", "cmsteam"}) {
		t.Fatalf("retired = %v", retired)
	}
	var reason string
	var group *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT reason, group_id FROM retired_team_codes WHERE code = 'GONE'`).Scan(&reason, &group); err != nil {
		t.Fatal(err)
	}
	if reason != teamcode.ReasonSeeded || group != nil {
		t.Fatalf("GONE retired as %q with group %v", reason, group)
	}
	if !held(t, ledger, "gone") {
		t.Fatal("a seeded code is not held")
	}

	// A failing read inside the lock writes nothing.
	boom := errors.New("keycloak down")
	if _, err := ledger.SeedRetired(ctx, func(context.Context) ([]string, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if _, err := ledger.SeedRetired(ctx, func(context.Context) ([]string, error) { return []string{"NEWCODE"}, boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if held(t, ledger, "NEWCODE") {
		t.Fatal("a failed seed wrote a code")
	}
}

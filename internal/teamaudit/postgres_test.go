package teamaudit_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
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

func memberAdd(actor, target uuid.UUID) teamaudit.Record {
	return teamaudit.Record{
		Actor:  teamaudit.Actor{ID: actor, Basis: teamaudit.BasisPrivileged, Client: "admin-panel", CorrelationID: "req-1"},
		Action: teamaudit.ActionMemberAdd, GroupID: uuid.NewString(), GroupPath: "/UYELER/ARGE/WEBLAB", TargetUserID: target,
	}
}

type stored struct {
	Outcome, ErrorCode, GroupID, Path, Client, Correlation string
	After                                                  *teamaudit.State
}

func read(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) stored {
	t.Helper()
	var s stored
	var after []byte
	if err := pool.QueryRow(context.Background(), `
		SELECT outcome, COALESCE(error_code, ''), COALESCE(group_id::text, ''), group_path, client, correlation_id, after
		FROM team_audit_records WHERE id = $1`, id).Scan(&s.Outcome, &s.ErrorCode, &s.GroupID, &s.Path, &s.Client, &s.Correlation, &after); err != nil {
		t.Fatal(err)
	}
	if after != nil {
		s.After = &teamaudit.State{}
		if err := json.Unmarshal(after, s.After); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// pending, then ok or failed; a create gets its Group's id when it is
// finished; denied is one row; a finished row is not finished again.
func TestPostgresWriterPendingThenOutcome(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	w := teamaudit.NewPostgres(pool)

	ok, err := w.Begin(ctx, memberAdd(uuid.New(), uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, pool, ok); got.Outcome != "pending" || got.ErrorCode != "" || got.Client != "admin-panel" || got.Correlation != "req-1" {
		t.Fatalf("begun %+v", got)
	}
	if err := w.Finish(ctx, ok, teamaudit.OutcomeOK, "", nil); err != nil {
		t.Fatal(err)
	}
	if got := read(t, pool, ok); got.Outcome != "ok" {
		t.Fatalf("finished %+v", got)
	}
	if err := w.Finish(ctx, ok, teamaudit.OutcomeFailed, "not_found", nil); !errors.Is(err, teamaudit.ErrNotPending) {
		t.Fatalf("finished twice: %v", err)
	}

	failed, err := w.Begin(ctx, memberAdd(uuid.New(), uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Finish(ctx, failed, teamaudit.OutcomeFailed, "not_found", nil); err != nil {
		t.Fatal(err)
	}
	if got := read(t, pool, failed); got.Outcome != "failed" || got.ErrorCode != "not_found" {
		t.Fatalf("failed %+v", got)
	}

	create := teamaudit.Record{
		Actor:  teamaudit.Actor{ID: uuid.New(), Basis: teamaudit.BasisPrivileged},
		Action: teamaudit.ActionTeamCreate, GroupPath: "/UYELER/ARGE/WEBLAB/AGC",
		After: &teamaudit.State{Path: "/UYELER/ARGE/WEBLAB/AGC", Code: "AGC"},
	}
	created, err := w.Begin(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, pool, created); got.GroupID != "" || got.After == nil || got.After.Code != "AGC" {
		t.Fatalf("create begun %+v", got)
	}
	groupID := uuid.NewString()
	if err := w.Finish(ctx, created, teamaudit.OutcomeOK, "", &teamaudit.State{ID: groupID, Path: "/UYELER/ARGE/WEBLAB/AGC", Code: "AGC"}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, pool, created); got.GroupID != groupID || got.After.ID != groupID {
		t.Fatalf("create finished %+v", got)
	}

	leader := memberAdd(uuid.New(), uuid.New())
	leader.Actor.Basis, leader.Actor.ViaGroupID = teamaudit.BasisLeader, uuid.NewString()
	if err := w.Deny(ctx, leader, "leader_group_needs_parent"); err != nil {
		t.Fatal(err)
	}
	var denied int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM team_audit_records WHERE outcome = 'denied' AND error_code = 'leader_group_needs_parent' AND actor_basis = 'leader'`).Scan(&denied); err != nil || denied != 1 {
		t.Fatalf("denied %d %v", denied, err)
	}

	// A caller with no basis for the change: denied only (ticket 04).
	nobody := memberAdd(uuid.New(), uuid.New())
	nobody.Actor.Basis = teamaudit.BasisNone
	if err := w.Deny(ctx, nobody, "forbidden"); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM team_audit_records WHERE outcome = 'denied' AND error_code = 'forbidden' AND actor_basis = 'none' AND actor_via_group_id IS NULL`).Scan(&denied); err != nil || denied != 1 {
		t.Fatalf("denied without a basis %d %v", denied, err)
	}
	if _, err := w.Begin(ctx, nobody); !errors.Is(err, teamaudit.ErrUnavailable) {
		t.Fatalf("begun without a basis: %v", err)
	}
}

// The table refuses what the writer never writes, and a finished row
// cannot be changed: only pending turns into its outcome.
func TestTeamAuditRecordsChecksAndFinishOnly(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	w := teamaudit.NewPostgres(pool)
	id, err := w.Begin(ctx, memberAdd(uuid.New(), uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO team_audit_records (id, occurred_at, actor_id, actor_basis, action, group_id, group_path, target_user_id, outcome, error_code)
		VALUES (gen_random_uuid(), now(), gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7)`
	group, target := uuid.New(), uuid.New()
	for name, args := range map[string][]any{
		"unknown action":         {"privileged", "member.promote", group, "/UYELER", target, "ok", nil},
		"unknown outcome":        {"privileged", "member.add", group, "/UYELER", target, "done", nil},
		"leader without a team":  {"leader", "member.add", group, "/UYELER", target, "ok", nil},
		"no basis, not denied":   {"none", "member.add", group, "/UYELER", target, "pending", nil},
		"no basis, ok":           {"none", "member.add", group, "/UYELER", target, "ok", nil},
		"unknown basis":          {"member", "member.add", group, "/UYELER", target, "denied", "forbidden"},
		"member without person":  {"privileged", "member.add", group, "/UYELER", nil, "ok", nil},
		"roles with a person":    {"privileged", "group.roles", group, "/UYELER", target, "ok", nil},
		"no group":               {"privileged", "member.add", nil, "/UYELER", target, "ok", nil},
		"created without its id": {"privileged", "team.create", nil, "/UYELER/X", nil, "ok", nil},
		"relative path":          {"privileged", "member.add", group, "UYELER", target, "ok", nil},
		"failed without a code":  {"privileged", "member.add", group, "/UYELER", target, "failed", nil},
		"ok with a code":         {"privileged", "member.add", group, "/UYELER", target, "ok", "not_found"},
		"code with a message":    {"privileged", "member.add", group, "/UYELER", target, "failed", "Ada Lovelace"},
	} {
		if _, err := pool.Exec(ctx, insert, args...); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := pool.Exec(ctx, insert, "privileged", "team.create", nil, "/UYELER/X", nil, "pending", nil); err != nil {
		t.Fatalf("a pending create without its id: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE team_audit_records SET group_path = '/OTHER' WHERE id = $1`, id); err == nil {
		t.Fatal("a pending row's path changed")
	}
	if err := w.Finish(ctx, id, teamaudit.OutcomeOK, "", nil); err != nil {
		t.Fatal(err)
	}
	for name, statement := range map[string]string{
		"outcome": `UPDATE team_audit_records SET outcome = 'failed', error_code = 'x' WHERE id = $1`,
		"actor":   `UPDATE team_audit_records SET actor_id = gen_random_uuid() WHERE id = $1`,
		"target":  `UPDATE team_audit_records SET target_user_id = gen_random_uuid() WHERE id = $1`,
		"back":    `UPDATE team_audit_records SET outcome = 'pending' WHERE id = $1`,
	} {
		if _, err := pool.Exec(ctx, statement, id); err == nil {
			t.Errorf("a finished row's %s changed", name)
		}
	}
}

// A database that is down, or a table that refuses the row, is
// ErrUnavailable, carrying a code and never a value of the row.
func TestPostgresWriterUnavailable(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	w := teamaudit.NewPostgres(pool)
	if _, err := pool.Exec(ctx, `ALTER TABLE team_audit_records RENAME TO team_audit_records_gone`); err != nil {
		t.Fatal(err)
	}
	r := memberAdd(uuid.New(), uuid.New())
	_, err := w.Begin(ctx, r)
	if !errors.Is(err, teamaudit.ErrUnavailable) || err.Error() != "teamaudit: audit record unavailable (sqlstate_42p01)" {
		t.Fatalf("missing table: %v", err)
	}
	if err := w.Deny(ctx, r, "forbidden"); !errors.Is(err, teamaudit.ErrUnavailable) {
		t.Fatalf("deny, missing table: %v", err)
	}
	pool.Close()
	start := time.Now()
	if _, err := w.Begin(ctx, r); !errors.Is(err, teamaudit.ErrUnavailable) {
		t.Fatalf("closed pool: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("Begin waited past its timeout")
	}
}

// Account erasure neither deletes nor changes a row the person is the
// actor or the target of (docs/team-audit.md): the ids stay and resolve to
// nobody once the account is gone.
func TestAccountErasureKeepsTeamAuditRows(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	users := user.NewPostgresStore(pool)
	subject, other := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{subject, other} {
		if _, _, err := user.NewService(users).Ensure(ctx, id, user.Profile{Email: id.String() + "@example.test"}); err != nil {
			t.Fatal(err)
		}
	}
	w := teamaudit.NewPostgres(pool)
	var ids []uuid.UUID
	for _, r := range []teamaudit.Record{memberAdd(subject, other), memberAdd(other, subject)} {
		id, err := w.Begin(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Finish(ctx, id, teamaudit.OutcomeOK, "", nil); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	snapshot := func() string {
		var s string
		if err := pool.QueryRow(ctx, `SELECT string_agg(row_to_json(r)::text, E'\n' ORDER BY id) FROM team_audit_records r`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()
	if _, err := users.RequestDeletion(ctx, subject, nil); err != nil {
		t.Fatal(err)
	}
	if err := users.AnonymizeAccount(ctx, subject, time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(); after != before {
		t.Fatalf("erasure changed the record:\n%s\n---\n%s", before, after)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM team_audit_records WHERE actor_id = $1 OR target_user_id = $1`, subject).Scan(&count); err != nil || count != 2 {
		t.Fatalf("rows of the erased person %d %v", count, err)
	}
}

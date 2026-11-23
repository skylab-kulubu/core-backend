package identity_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// directoryRows stores n active people in core alone (none in Keycloak: the
// directory never reads it), one without a school e-mail, and one person
// being erased and one erased, whom the directory leaves out.
func directoryRows(t *testing.T, store *user.MemoryStore, n int) []uuid.UUID {
	t.Helper()
	ctx := context.Background()
	ids := make([]uuid.UUID, 0, n)
	for i := range n {
		id := uuid.New()
		u := user.User{
			ID: id, Email: id.String() + "@example.test", FirstName: "Ada", LastName: "Lovelace",
			Username: "ada" + id.String()[:8], SkyNumber: "SKY-" + id.String()[:8], Phone: "+905551112233",
		}
		if i > 0 {
			u.SchoolEmail = "okul" + id.String()[:8] + "@std.yildiz.edu.tr"
		}
		if _, _, err := store.Upsert(ctx, u); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	for _, state := range []user.AccountState{user.AccountDeletionPending, user.AccountAnonymized} {
		id := uuid.New()
		if _, _, err := store.Upsert(ctx, user.User{ID: id, Email: id.String() + "@gone.test", FirstName: "Ada", LastName: "Lovelace"}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.RequestDeletion(ctx, id, nil); err != nil {
			t.Fatal(err)
		}
		if state == user.AccountAnonymized {
			if err := store.AnonymizeAccount(ctx, id, time.Now().UTC(), nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	return ids
}

func directoryService(store user.Store, now func() time.Time) identity.Service {
	return identity.NewServiceWithOptions(identity.NewMemory(), store, authz.NewAuthorizer(authz.DefaultPolicy()), identity.Options{Now: now})
}

func TestService_UserDirectoryPagesTheActiveRowsNewestFirst(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	ids := directoryRows(t, store, 5)
	svc := directoryService(store, nil)
	ctx := context.Background()

	var seen []identity.DirectoryPerson
	for page := 1; page <= 3; page++ {
		got, err := svc.UserDirectory(ctx, privileged(), identity.UserDirectoryQuery{Page: page, Size: 2})
		if err != nil {
			t.Fatal(err)
		}
		if got.Total != 5 || got.Page != page || got.Size != 2 {
			t.Fatalf("page %d = total %d page %d size %d", page, got.Total, got.Page, got.Size)
		}
		if want := min(2, 5-(page-1)*2); len(got.Items) != want {
			t.Fatalf("page %d holds %d, want %d", page, len(got.Items), want)
		}
		seen = append(seen, got.Items...)
	}
	got := make([]uuid.UUID, 0, len(seen))
	for _, p := range seen {
		got = append(got, p.ID)
		if p.Status != user.ReadStatusActive || p.DisplayName != "Ada Lovelace" || p.SkyNumber == "" || p.Username == "" || p.CreatedAt == nil {
			t.Fatalf("full entry = %+v", p)
		}
	}
	if !slices.IsSortedFunc(seen, func(a, b identity.DirectoryPerson) int {
		if c := b.CreatedAt.Compare(*a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID.String(), b.ID.String())
	}) {
		t.Fatalf("not newest first: %+v", seen)
	}
	slices.SortFunc(got, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	if !slices.Equal(got, ids) {
		t.Fatalf("pages = %v, want every active row once %v", got, ids)
	}

	past, err := svc.UserDirectory(ctx, privileged(), identity.UserDirectoryQuery{Page: 9, Size: 2})
	if err != nil || past.Total != 5 || len(past.Items) != 0 {
		t.Fatalf("page past the end = %+v, %v", past, err)
	}
}

func TestService_UserDirectorySizeDefaultsAndCaps(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	directoryRows(t, store, 1)
	svc := directoryService(store, nil)
	for size, want := range map[int]int{0: identity.DirectoryDefaultSize, 7: 7, 500: identity.DirectoryMaxSize} {
		got, err := svc.UserDirectory(context.Background(), privileged(), identity.UserDirectoryQuery{Page: 1, Size: size})
		if err != nil || got.Size != want {
			t.Fatalf("size %d = %d, %v; want %d", size, got.Size, err, want)
		}
	}
}

func TestService_UserDirectorySearchesAndFilters(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	ids := directoryRows(t, store, 3)
	ctx := context.Background()
	svc := directoryService(store, nil)

	only := func(t *testing.T, got identity.UserDirectoryPage, err error, want uuid.UUID) {
		t.Helper()
		if err != nil || got.Total != 1 || len(got.Items) != 1 || got.Items[0].ID != want {
			t.Fatalf("got %+v, %v; want only %s", got, err, want)
		}
	}
	got, err := svc.UserDirectory(ctx, privileged(), identity.UserDirectoryQuery{Page: 1, Filter: identity.DirectoryFilterNoSchoolEmail})
	only(t, got, err, ids[0])
	got, err = svc.UserDirectory(ctx, privileged(), identity.UserDirectoryQuery{Page: 1, Query: strings.ToUpper("SKY-" + ids[1].String()[:8])})
	only(t, got, err, ids[1])
	got, err = svc.UserDirectory(ctx, privileged(), identity.UserDirectoryQuery{Page: 1, Query: "okul" + ids[2].String()[:8]})
	only(t, got, err, ids[2])

	got, err = svc.UserDirectory(ctx, privileged(), identity.UserDirectoryQuery{Page: 1, Filter: identity.DirectoryFilterNew})
	if err != nil || got.Total != 3 {
		t.Fatalf("new this month = %+v, %v", got, err)
	}
	nextMonth := directoryService(store, func() time.Time { return time.Now().AddDate(0, 1, 0) })
	got, err = nextMonth.UserDirectory(ctx, privileged(), identity.UserDirectoryQuery{Page: 1, Filter: identity.DirectoryFilterNew})
	if err != nil || got.Total != 0 {
		t.Fatalf("new next month = %+v, %v", got, err)
	}
}

// A users:read caller without ReadFull reads the entries GET /v1/users
// answers it, searches the e-mail and the names alone, and may not filter
// on what those entries leave out.
func TestService_UserDirectoryKeepsTheLimitedReadersView(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	ids := directoryRows(t, store, 2)
	ctx := context.Background()
	svc := directoryService(store, nil)

	got, err := svc.UserDirectory(ctx, formsReader(), identity.UserDirectoryQuery{Page: 1})
	if err != nil || got.Total != 2 {
		t.Fatalf("limited list = %+v, %v", got, err)
	}
	for _, p := range got.Items {
		if p.Email == "" || p.FirstName != "Ada" || p.Status != user.ReadStatusActive ||
			p.SchoolEmail != "" || p.SkyNumber != "" || p.Username != "" || p.CreatedAt != nil {
			t.Fatalf("limited entry = %+v", p)
		}
	}
	got, err = svc.UserDirectory(ctx, formsReader(), identity.UserDirectoryQuery{Page: 1, Query: "okul" + ids[1].String()[:8]})
	if err != nil || got.Total != 0 {
		t.Fatalf("limited search on the school e-mail = %+v, %v", got, err)
	}
	got, err = svc.UserDirectory(ctx, formsReader(), identity.UserDirectoryQuery{Page: 1, Query: ids[1].String()})
	if err != nil || got.Total != 1 || got.Items[0].ID != ids[1] {
		t.Fatalf("limited search on the e-mail = %+v, %v", got, err)
	}
	for _, filter := range []string{identity.DirectoryFilterNoSchoolEmail, identity.DirectoryFilterNew} {
		if _, err := svc.UserDirectory(ctx, formsReader(), identity.UserDirectoryQuery{Page: 1, Filter: filter}); !errors.Is(err, identity.ErrForbidden) {
			t.Fatalf("limited filter %s err = %v", filter, err)
		}
	}
}

func TestService_UserDirectoryRefusesBeforeItReads(t *testing.T) {
	t.Parallel()
	svc := directoryService(user.NewMemoryStore(), nil)
	ctx := context.Background()
	// No users:read: 403 whatever the query, a broken one too; the Forms
	// seat search has no counterpart here.
	for _, q := range []identity.UserDirectoryQuery{{Page: 1}, {Page: 0, Malformed: true}} {
		if _, err := svc.UserDirectory(ctx, member(), q); !errors.Is(err, identity.ErrForbidden) {
			t.Fatalf("member %+v err = %v", q, err)
		}
	}
	// A page past the last one is refused before its offset is worked out
	if _, err := svc.UserDirectory(ctx, privileged(), identity.UserDirectoryQuery{Page: identity.DirectoryMaxPage + 1, Size: 100}); !errors.Is(err, identity.ErrDirectoryPage) || !errors.Is(err, identity.ErrInvalid) {
		t.Fatalf("page past the last err = %v", err)
	}
	if _, err := svc.UserDirectory(ctx, member(), identity.UserDirectoryQuery{Page: identity.DirectoryMaxPage + 1}); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("member past the last page err = %v", err)
	}
	if got, err := svc.UserDirectory(ctx, privileged(), identity.UserDirectoryQuery{Page: identity.DirectoryMaxPage, Size: 100}); err != nil || len(got.Items) != 0 {
		t.Fatalf("last page: %+v, %v", got, err)
	}
	for _, q := range []identity.UserDirectoryQuery{{Page: 0}, {Page: 1, Malformed: true}, {Page: 1, Filter: "everyone"}} {
		if _, err := svc.UserDirectory(ctx, privileged(), q); !errors.Is(err, identity.ErrInvalid) {
			t.Fatalf("%+v err = %v", q, err)
		}
	}
}

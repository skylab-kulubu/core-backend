package user_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
	"github.com/skylab-kulubu/core-backend/internal/ytu"
)

func TestPostgresYTUProfile(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	users := user.NewService(store)
	id := uuid.New()
	login := user.Profile{
		Email: "ytu@std.yildiz.edu.tr", FirstName: "Ada", LastName: "Lovelace",
		University: "Yıldız Teknik Üniversitesi", Department: "YapayZekaveVeriMühendisliği",
	}

	got, _, err := users.Ensure(ctx, id, login)
	if err != nil {
		t.Fatal(err)
	}
	want := func(u user.User, department, faculty string) {
		t.Helper()
		if !u.YTULinked || u.University != "Yıldız Teknik Üniversitesi" || u.Department != department || u.Faculty != faculty {
			t.Fatalf("got %+v, want %q / %q", u, department, faculty)
		}
	}
	want(got, "Yapay Zeka ve Veri Mühendisliği", "Bilgisayar ve Bilişim Bilimleri Fakültesi")
	stored, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	want(stored, "Yapay Zeka ve Veri Mühendisliği", "Bilgisayar ve Bilişim Bilimleri Fakültesi")

	// A request whose token has no YTÜ claims keeps the flag and the values.
	again, _, err := users.Ensure(ctx, id, user.Profile{Email: login.Email})
	if err != nil {
		t.Fatal(err)
	}
	want(again, "Yapay Zeka ve Veri Mühendisliği", "Bilgisayar ve Bilişim Bilimleri Fakültesi")

	// A profile write built from a stale read cannot revert the YTÜ values.
	stale := stored
	stale.University, stale.Faculty, stale.Department = "x", "y", "z"
	stale.Linkedin = "https://www.linkedin.com/in/ada"
	updated, err := store.UpdateProfile(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	want(updated, "Yapay Zeka ve Veri Mühendisliği", "Bilgisayar ve Bilişim Bilimleri Fakültesi")
	if updated.Linkedin != stale.Linkedin {
		t.Fatalf("linkedin %q", updated.Linkedin)
	}

	// A transfer on the next login.
	login.Department = "058"
	moved, _, err := users.Ensure(ctx, id, login)
	if err != nil {
		t.Fatal(err)
	}
	want(moved, "Matematik Mühendisliği (İngilizce)", "Bilgisayar ve Bilişim Bilimleri Fakültesi")

	other := "Fizik"
	if _, err := users.Patch(ctx, id, user.ProfilePatch{Department: &other}); !errors.Is(err, user.ErrYTUManaged) {
		t.Fatalf("patch department err=%v", err)
	}

	if _, err := store.SetYTUProfile(ctx, uuid.New(), ytu.Profile{University: "Yıldız Teknik Üniversitesi"}); !errors.Is(err, user.ErrNotFound) {
		t.Fatalf("unknown id err=%v", err)
	}

	// Erasure forgets the link together with the values.
	if _, err := store.RequestDeletion(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetYTUProfile(ctx, id, ytu.Profile{University: "Yıldız Teknik Üniversitesi"}); !errors.Is(err, user.ErrAccountBlocked) {
		t.Fatalf("pending deletion err=%v", err)
	}
	if err := store.AnonymizeAccount(ctx, id, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var linked bool
	var university, faculty, department string
	if err := pool.QueryRow(ctx, `SELECT ytu_linked, university, faculty, department FROM users WHERE id = $1`, id).
		Scan(&linked, &university, &faculty, &department); err != nil {
		t.Fatal(err)
	}
	if linked || university != "" || faculty != "" || department != "" {
		t.Fatalf("anonymized row keeps YTÜ data: linked=%v %q %q %q", linked, university, faculty, department)
	}
}

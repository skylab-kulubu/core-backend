package user

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestService_EnsureCreatesThenUpdates(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	svc := NewService(store)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	ctx := context.Background()

	first, created, err := svc.Ensure(ctx, id, Profile{Email: "a@example.com", FirstName: "Ada", LastName: "Lovelace"})
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first ensure should create")
	}
	if first.Email != "a@example.com" || first.FirstName != "Ada" || first.ID != id {
		t.Fatalf("unexpected first upsert: %+v", first)
	}

	second, created, err := svc.Ensure(ctx, id, Profile{Email: "b@example.com", FirstName: "Ada", LastName: "Byron"})
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("second ensure should update")
	}
	if second.Email != "b@example.com" || second.LastName != "Byron" {
		t.Fatalf("unexpected second upsert: %+v", second)
	}

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "b@example.com" {
		t.Fatalf("store has %q", got.Email)
	}
}

func TestService_EnsureKeepsSchoolEmailWhenClaimEmpty(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	svc := NewService(store)
	id := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	ctx := context.Background()

	if _, _, err := svc.Ensure(ctx, id, Profile{Email: "a@example.com", SchoolEmail: "a@std.yildiz.edu.tr"}); err != nil {
		t.Fatal(err)
	}
	second, _, err := svc.Ensure(ctx, id, Profile{Email: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if second.SchoolEmail != "a@std.yildiz.edu.tr" {
		t.Fatalf("wiped school email: %+v", second)
	}

	hit, err := store.Search(ctx, "std.yildiz")
	if err != nil {
		t.Fatal(err)
	}
	if len(hit) != 1 || hit[0].ID != id {
		t.Fatalf("search %+v", hit)
	}
}

func TestService_EnsureConcurrentSameID(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	svc := NewService(store)
	id := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := svc.Ensure(ctx, id, Profile{Email: "c@example.com", FirstName: "Grace", LastName: "Hopper"})
			if err != nil {
				t.Errorf("Ensure: %v", err)
			}
		}()
	}
	wg.Wait()

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "c@example.com" {
		t.Fatalf("got %+v", got)
	}
}

func TestService_EnsureAssignsSkyNumberOnce(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	svc := NewService(store)
	id := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	ctx := context.Background()

	first, created, err := svc.Ensure(ctx, id, Profile{Email: "a@example.com", FirstName: "Ada"})
	if err != nil {
		t.Fatal(err)
	}
	if !created || first.SkyNumber != "SKY-0000001" {
		t.Fatalf("first %+v created=%v", first, created)
	}

	second, created, err := svc.Ensure(ctx, id, Profile{Email: "a@example.com", FirstName: "Ada"})
	if err != nil {
		t.Fatal(err)
	}
	if created || second.SkyNumber != "SKY-0000001" {
		t.Fatalf("second %+v created=%v", second, created)
	}
}

func TestService_EnsureKeepsClaimedSkyNumber(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	svc := NewService(store)
	id := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	got, _, err := svc.Ensure(context.Background(), id, Profile{Email: "a@example.com", SkyNumber: "SKY-0000042"})
	if err != nil {
		t.Fatal(err)
	}
	if got.SkyNumber != "SKY-0000042" {
		t.Fatalf("got %+v", got)
	}
}

func TestService_EnsureDistinctSkyNumbers(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	svc := NewService(store)
	a := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	b := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	first, _, err := svc.Ensure(context.Background(), a, Profile{Email: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := svc.Ensure(context.Background(), b, Profile{Email: "b@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if first.SkyNumber == second.SkyNumber || first.SkyNumber == "" || second.SkyNumber == "" {
		t.Fatalf("%q %q", first.SkyNumber, second.SkyNumber)
	}
}

type recSky struct {
	n string
}

func (r *recSky) ReadSkyNumber(context.Context, uuid.UUID) (string, error) {
	return r.n, nil
}

func (r *recSky) WriteSkyNumber(_ context.Context, _ uuid.UUID, n string) error {
	r.n = n
	return nil
}

func TestService_EnsureReusesDirectorySkyNumber(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	sync := &recSky{n: "SKY-0000099"}
	svc := NewService(store, sync)
	id := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	got, _, err := svc.Ensure(context.Background(), id, Profile{Email: "c@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if got.SkyNumber != "SKY-0000099" {
		t.Fatalf("got %+v", got)
	}
}

func TestService_ReplaceAndPatchKeepSkyNumber(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	svc := NewService(store)
	id := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	ctx := context.Background()
	if _, _, err := svc.Ensure(ctx, id, Profile{Email: "a@example.com", FirstName: "Ada", LastName: "Lovelace", SchoolEmail: "a@std.yildiz.edu.tr"}); err != nil {
		t.Fatal(err)
	}

	replaced, err := svc.Replace(ctx, id, ProfileUpdate{
		FirstName:  "Ada",
		LastName:   "Byron",
		Linkedin:   "https://linkedin.com/in/ada",
		University: "YTÜ",
		Faculty:    "EE",
		Department: "CE",
	})
	if err != nil {
		t.Fatal(err)
	}
	if replaced.SkyNumber != "SKY-0000001" || replaced.SchoolEmail != "a@std.yildiz.edu.tr" {
		t.Fatalf("wiped identity %+v", replaced)
	}
	if replaced.LastName != "Byron" || replaced.Linkedin != "https://linkedin.com/in/ada" || replaced.Department != "CE" {
		t.Fatalf("replace %+v", replaced)
	}

	empty := ""
	patched, err := svc.Patch(ctx, id, ProfilePatch{Linkedin: &empty, University: ptr("İTÜ")})
	if err != nil {
		t.Fatal(err)
	}
	if patched.Linkedin != "" || patched.University != "İTÜ" || patched.Faculty != "EE" || patched.SkyNumber != "SKY-0000001" {
		t.Fatalf("patch %+v", patched)
	}

	again, _, err := svc.Ensure(ctx, id, Profile{Email: "a@example.com", FirstName: "Ada", LastName: "Byron"})
	if err != nil {
		t.Fatal(err)
	}
	if again.University != "İTÜ" || again.Faculty != "EE" || again.Department != "CE" || again.SkyNumber != "SKY-0000001" {
		t.Fatalf("ensure wiped profile %+v", again)
	}
}

func TestService_ReplaceRequiresNames(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	svc := NewService(store)
	id := uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	if _, _, err := svc.Ensure(context.Background(), id, Profile{Email: "a@example.com", FirstName: "Ada", LastName: "Lovelace"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Replace(context.Background(), id, ProfileUpdate{FirstName: "Ada"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("got %v", err)
	}
}

func TestService_SetProfilePicture(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	svc := NewService(store)
	id := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	if _, _, err := svc.Ensure(context.Background(), id, Profile{Email: "a@example.com", FirstName: "Ada", LastName: "Lovelace"}); err != nil {
		t.Fatal(err)
	}
	mediaID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	got, err := svc.SetProfilePicture(context.Background(), id, mediaID, "https://cdn.example.test/pic")
	if err != nil {
		t.Fatal(err)
	}
	if got.ProfilePictureID == nil || *got.ProfilePictureID != mediaID || got.ProfilePictureURL != "https://cdn.example.test/pic" {
		t.Fatalf("picture %+v", got)
	}
	again, _, err := svc.Ensure(context.Background(), id, Profile{Email: "a@example.com", FirstName: "Ada", LastName: "Lovelace"})
	if err != nil {
		t.Fatal(err)
	}
	if again.ProfilePictureURL != "https://cdn.example.test/pic" {
		t.Fatalf("ensure wiped picture %+v", again)
	}
}

func TestService_EnsureKeepsUsername(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	svc := NewService(store)
	id := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	ctx := context.Background()
	first, _, err := svc.Ensure(ctx, id, Profile{Email: "a@example.com", Username: "ada"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Username != "ada" {
		t.Fatalf("first %+v", first)
	}
	second, _, err := svc.Ensure(ctx, id, Profile{Email: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Username != "ada" {
		t.Fatalf("wiped username %+v", second)
	}
}

func ptr(s string) *string { return &s }

package user

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/ytu"
)

type Service interface {
	Ensure(ctx context.Context, id uuid.UUID, profile Profile) (User, bool, error)
	Replace(ctx context.Context, id uuid.UUID, in ProfileUpdate) (User, error)
	Patch(ctx context.Context, id uuid.UUID, in ProfilePatch) (User, error)
	SetProfilePicture(ctx context.Context, id uuid.UUID, mediaID uuid.UUID, url string) (User, error)
	// ClearProfilePicture unlinks the person's own profile picture and reports
	// which media was linked so the caller can release it. A profile without a
	// picture is left untouched and reports nil, so repeating the call is safe.
	// The store enforces the active-account rule on the write like every other
	// self-write; the service only repeats it so the early no-op path refuses a
	// blocked account too.
	ClearProfilePicture(ctx context.Context, id uuid.UUID) (*uuid.UUID, error)
}

type service struct {
	store Store
	sync  SkySync
}

func NewService(store Store, syncs ...SkySync) Service {
	s := &service{store: store}
	if len(syncs) > 0 {
		s.sync = syncs[0]
	}
	return s
}

func (s *service) Ensure(ctx context.Context, id uuid.UUID, profile Profile) (User, bool, error) {
	if profile.SkyNumber == "" && s.sync != nil {
		if n, err := s.sync.ReadSkyNumber(ctx, id); err == nil && n != "" {
			profile.SkyNumber = n
		}
	}
	put := func(sky string) (User, bool, error) {
		return s.store.Upsert(ctx, User{
			ID: id, Email: profile.Email, FirstName: profile.FirstName, LastName: profile.LastName,
			Username: profile.Username, SchoolEmail: profile.SchoolEmail, SkyNumber: sky,
		})
	}
	first, created, err := put(profile.SkyNumber)
	if errors.Is(err, ErrConflict) {
		first, created, err = put("")
	}
	if errors.Is(err, ErrConflict) && profile.Email != "" {
		found, e2 := s.store.FindByEmail(ctx, profile.Email)
		if e2 == nil {
			for _, existing := range found {
				if existing.ID != id {
					return existing, false, nil
				}
			}
		}
	}
	if err != nil {
		return User{}, false, err
	}
	u, created, err := s.assignSky(ctx, first, created)
	if err != nil {
		return User{}, false, err
	}
	if p, linked := ytu.FromClaims(profile.University, profile.Department); linked {
		if u, err = s.syncYTU(ctx, u, p); err != nil {
			return User{}, false, err
		}
	}
	return u, created, nil
}

// syncYTU stores what the YTÜ Microsoft login says when it differs from the
// record. Every YTÜ login carries the current values (people change
// department), and most requests change nothing, so it compares first and
// writes only a difference.
func (s *service) syncYTU(ctx context.Context, u User, p ytu.Profile) (User, error) {
	if u.YTULinked && u.University == p.University && u.Faculty == p.Faculty && u.Department == p.Department {
		return u, nil
	}
	return s.store.SetYTUProfile(ctx, u.ID, p)
}

// ytuEdit decides a self or admin edit of the three YTÜ fields. It reports
// whether the record takes the edited values: a person who is not
// YTÜ-linked edits freely; for a YTÜ-linked person the values follow the
// login, so sending the stored value back is fine and anything else is
// ErrYTUManaged.
func ytuEdit(existing User, university, faculty, department *string) (bool, error) {
	if !existing.YTULinked {
		return true, nil
	}
	same := func(in *string, stored string) bool { return in == nil || strings.TrimSpace(*in) == stored }
	if same(university, existing.University) && same(faculty, existing.Faculty) && same(department, existing.Department) {
		return false, nil
	}
	return false, ErrYTUManaged
}

func (s *service) assignSky(ctx context.Context, first User, created bool) (User, bool, error) {
	if first.SkyNumber != "" {
		return first, created, nil
	}
	assigned := first
	for i := 0; i < 8; i++ {
		n, err := s.store.NextSkyNumber(ctx)
		if err != nil {
			return User{}, false, err
		}
		assigned.SkyNumber = n
		got, _, err := s.store.Upsert(ctx, assigned)
		if err == nil {
			if s.sync != nil {
				_ = s.sync.WriteSkyNumber(ctx, got.ID, got.SkyNumber)
			}
			return got, created, nil
		}
		if !errors.Is(err, ErrConflict) {
			return User{}, false, err
		}
	}
	return User{}, false, ErrConflict
}

func (s *service) Replace(ctx context.Context, id uuid.UUID, in ProfileUpdate) (User, error) {
	if in.FirstName == "" || in.LastName == "" {
		return User{}, ErrInvalid
	}
	existing, err := s.store.Get(ctx, id)
	if err != nil {
		return User{}, err
	}
	editable, err := ytuEdit(existing, &in.University, &in.Faculty, &in.Department)
	if err != nil {
		return User{}, err
	}
	existing.FirstName = in.FirstName
	existing.LastName = in.LastName
	existing.Linkedin = in.Linkedin
	if editable {
		existing.University = in.University
		existing.Faculty = in.Faculty
		existing.Department = in.Department
	}
	return s.store.UpdateProfile(ctx, existing)
}

func (s *service) Patch(ctx context.Context, id uuid.UUID, in ProfilePatch) (User, error) {
	existing, err := s.store.Get(ctx, id)
	if err != nil {
		return User{}, err
	}
	if in.FirstName != nil {
		existing.FirstName = *in.FirstName
	}
	if in.LastName != nil {
		existing.LastName = *in.LastName
	}
	editable, err := ytuEdit(existing, in.University, in.Faculty, in.Department)
	if err != nil {
		return User{}, err
	}
	if in.Linkedin != nil {
		existing.Linkedin = *in.Linkedin
	}
	if editable && in.University != nil {
		existing.University = *in.University
	}
	if editable && in.Faculty != nil {
		existing.Faculty = *in.Faculty
	}
	if editable && in.Department != nil {
		existing.Department = *in.Department
	}
	if in.Phone != nil {
		existing.Phone = *in.Phone
	}
	return s.store.UpdateProfile(ctx, existing)
}

func (s *service) SetProfilePicture(ctx context.Context, id uuid.UUID, mediaID uuid.UUID, url string) (User, error) {
	existing, err := s.store.Get(ctx, id)
	if err != nil {
		return User{}, err
	}
	existing.ProfilePictureID = &mediaID
	existing.ProfilePictureURL = url
	return s.store.UpdateProfile(ctx, existing)
}

func (s *service) ClearProfilePicture(ctx context.Context, id uuid.UUID) (*uuid.UUID, error) {
	existing, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if existing.AccountState != AccountActive {
		return nil, ErrAccountBlocked
	}
	if existing.ProfilePictureID == nil && existing.ProfilePictureURL == "" {
		return nil, nil
	}
	released := existing.ProfilePictureID
	existing.ProfilePictureID = nil
	existing.ProfilePictureURL = ""
	if _, err := s.store.UpdateProfile(ctx, existing); err != nil {
		return nil, err
	}
	return released, nil
}

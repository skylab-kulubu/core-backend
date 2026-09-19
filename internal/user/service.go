package user

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

type Service interface {
	Ensure(ctx context.Context, id uuid.UUID, profile Profile) (User, bool, error)
	Replace(ctx context.Context, id uuid.UUID, in ProfileUpdate) (User, error)
	Patch(ctx context.Context, id uuid.UUID, in ProfilePatch) (User, error)
	SetProfilePicture(ctx context.Context, id uuid.UUID, mediaID uuid.UUID, url string) (User, error)
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
	return s.assignSky(ctx, first, created)
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
	existing.FirstName = in.FirstName
	existing.LastName = in.LastName
	existing.Linkedin = in.Linkedin
	existing.University = in.University
	existing.Faculty = in.Faculty
	existing.Department = in.Department
	return s.store.UpdateProfile(ctx, existing)
}

func (s *service) Patch(ctx context.Context, id uuid.UUID, in ProfilePatch) (User, error) {
	existing, err := s.store.Get(ctx, id)
	if err != nil {
		return User{}, err
	}
	if in.FirstName != nil && *in.FirstName != "" {
		existing.FirstName = *in.FirstName
	}
	if in.LastName != nil && *in.LastName != "" {
		existing.LastName = *in.LastName
	}
	if in.Linkedin != nil {
		existing.Linkedin = *in.Linkedin
	}
	if in.University != nil {
		existing.University = *in.University
	}
	if in.Faculty != nil {
		existing.Faculty = *in.Faculty
	}
	if in.Department != nil {
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

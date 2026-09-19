package user

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type MemoryStore struct {
	mu   sync.Mutex
	byID map[uuid.UUID]User
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{byID: make(map[uuid.UUID]User)}
}

func (s *MemoryStore) Get(_ context.Context, id uuid.UUID) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byID[id]
	if !ok {
		return User{}, ErrNotFound
	}
	return u, nil
}

func keepProfile(existing, u User) User {
	u.FirstName = existing.FirstName
	u.LastName = existing.LastName
	if u.SchoolEmail == "" {
		u.SchoolEmail = existing.SchoolEmail
	}
	if u.SkyNumber == "" {
		u.SkyNumber = existing.SkyNumber
	}
	if u.Username == "" {
		u.Username = existing.Username
	}
	if u.Linkedin == "" {
		u.Linkedin = existing.Linkedin
	}
	if u.University == "" {
		u.University = existing.University
	}
	if u.Faculty == "" {
		u.Faculty = existing.Faculty
	}
	if u.Department == "" {
		u.Department = existing.Department
	}
	if u.Phone == "" {
		u.Phone = existing.Phone
	}
	if u.StudentCardUID == "" {
		u.StudentCardUID = existing.StudentCardUID
	}
	if u.ProfilePictureID == nil {
		u.ProfilePictureID = existing.ProfilePictureID
	}
	if u.ProfilePictureURL == "" {
		u.ProfilePictureURL = existing.ProfilePictureURL
	}
	return u
}

func (s *MemoryStore) Upsert(_ context.Context, u User) (User, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	existing, existed := s.byID[u.ID]
	if existed {
		u = keepProfile(existing, u)
		u.CreatedAt = existing.CreatedAt
		u.UpdatedAt = now
	} else {
		u.CreatedAt = now
		u.UpdatedAt = now
	}
	if u.Email != "" {
		want := strings.ToLower(u.Email)
		for id, other := range s.byID {
			if id != u.ID && strings.ToLower(other.Email) == want {
				return User{}, false, ErrConflict
			}
		}
	}
	if u.SkyNumber != "" {
		for id, other := range s.byID {
			if id != u.ID && other.SkyNumber == u.SkyNumber {
				return User{}, false, ErrConflict
			}
		}
	}
	if u.StudentCardUID != "" {
		for id, other := range s.byID {
			if id != u.ID && other.StudentCardUID == u.StudentCardUID {
				return User{}, false, ErrConflict
			}
		}
	}
	s.byID[u.ID] = u
	return u, !existed, nil
}

func (s *MemoryStore) UpdateProfile(_ context.Context, u User) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.byID[u.ID]
	if !ok {
		return User{}, ErrNotFound
	}
	existing.FirstName = u.FirstName
	existing.LastName = u.LastName
	existing.Linkedin = u.Linkedin
	existing.University = u.University
	existing.Faculty = u.Faculty
	existing.Department = u.Department
	existing.Phone = u.Phone
	existing.StudentCardUID = u.StudentCardUID
	existing.ProfilePictureID = u.ProfilePictureID
	existing.ProfilePictureURL = u.ProfilePictureURL
	existing.UpdatedAt = time.Now().UTC()
	s.byID[u.ID] = existing
	return existing, nil
}

func (s *MemoryStore) Search(_ context.Context, q string) ([]User, error) {
	return s.search(q, 0), nil
}

func (s *MemoryStore) SearchLimit(_ context.Context, q string, limit int) ([]User, error) {
	return s.search(q, limit), nil
}

func (s *MemoryStore) search(q string, limit int) []User {
	s.mu.Lock()
	defer s.mu.Unlock()
	needle := strings.ToLower(strings.TrimSpace(q))
	out := make([]User, 0)
	for _, u := range s.byID {
		if userMatches(u, needle) {
			out = append(out, u)
			if limit > 0 && len(out) == limit {
				break
			}
		}
	}
	return out
}

func (s *MemoryStore) FindByEmail(_ context.Context, email string) ([]User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := strings.ToLower(strings.TrimSpace(email))
	out := make([]User, 0)
	if want == "" {
		return out, nil
	}
	for _, u := range s.byID {
		if strings.ToLower(u.Email) == want {
			out = append(out, u)
		}
	}
	return out, nil
}

func (s *MemoryStore) FindByStudentCardUID(_ context.Context, uid string) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if uid == "" {
		return User{}, ErrNotFound
	}
	for _, u := range s.byID {
		if u.StudentCardUID == uid {
			return u, nil
		}
	}
	return User{}, ErrNotFound
}

func (s *MemoryStore) SetStudentCardUID(_ context.Context, id uuid.UUID, uid string) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byID[id]
	if !ok {
		return User{}, ErrNotFound
	}
	if uid != "" {
		for otherID, other := range s.byID {
			if otherID != id && other.StudentCardUID == uid {
				return User{}, ErrConflict
			}
		}
	}
	u.StudentCardUID = uid
	u.UpdatedAt = time.Now().UTC()
	s.byID[id] = u
	return u, nil
}

func (s *MemoryStore) NextSkyNumber(_ context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	max := 0
	for _, u := range s.byID {
		if n, ok := parseSkyNumber(u.SkyNumber); ok && n > max {
			max = n
		}
	}
	return FormatSkyNumber(max + 1)
}

func (s *MemoryStore) Delete(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[id]; !ok {
		return ErrNotFound
	}
	delete(s.byID, id)
	return nil
}

func userMatches(u User, needle string) bool {
	if needle == "" {
		return true
	}
	hay := []string{
		u.Email,
		u.SchoolEmail,
		u.SkyNumber,
		u.Username,
		u.FirstName,
		u.LastName,
		strings.TrimSpace(u.FirstName + " " + u.LastName),
	}
	for _, h := range hay {
		if strings.Contains(strings.ToLower(h), needle) {
			return true
		}
	}
	return false
}

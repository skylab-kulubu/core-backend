package user

import (
	"context"
	"crypto/subtle"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/ytu"
)

type MemoryStore struct {
	mu                  sync.Mutex
	byID                map[uuid.UUID]User
	deletionRequests    map[uuid.UUID]DeletionRequest
	deletionSteps       map[uuid.UUID]map[DeletionStep]time.Time
	selfDeletionIntakes map[uuid.UUID]SelfDeletionRecord
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		byID:                make(map[uuid.UUID]User),
		deletionRequests:    make(map[uuid.UUID]DeletionRequest),
		deletionSteps:       make(map[uuid.UUID]map[DeletionStep]time.Time),
		selfDeletionIntakes: make(map[uuid.UUID]SelfDeletionRecord),
	}
}

func (s *MemoryStore) Get(_ context.Context, id uuid.UUID) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byID[id]
	if !ok {
		return User{}, ErrNotFound
	}
	return withStudentCardStatus(u), nil
}

func (s *MemoryStore) CanAttribute(ctx context.Context, id uuid.UUID) (bool, error) {
	state, err := s.AttributionState(ctx, id)
	return state == AttributionAllowed, err
}

func (s *MemoryStore) AttributionState(_ context.Context, id uuid.UUID) (AttributionState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, blocked := s.deletionRequests[id]; blocked {
		return AttributionBlocked, nil
	}
	u, exists := s.byID[id]
	if exists && u.AccountState != AccountActive {
		return AttributionBlocked, nil
	}
	if exists {
		return AttributionAllowed, nil
	}
	// The in-memory store has no external directory to distinguish a first
	// authenticated visit from a nonexistent identity. Preserve its established
	// behavior and allow attribution unless a durable block is known.
	return AttributionAllowed, nil
}

func keepProfile(existing, u User) User {
	u.AccountState = existing.AccountState
	u.DeletionRequestedAt = existing.DeletionRequestedAt
	u.AnonymizedAt = existing.AnonymizedAt
	u.FirstName = existing.FirstName
	u.LastName = existing.LastName
	// Account Center's token carries no e-mail claim; keep the stored address.
	if u.Email == "" {
		u.Email = existing.Email
	}
	u.YTULinked = existing.YTULinked
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
		if existing.AccountState != AccountActive {
			return User{}, false, ErrAccountBlocked
		}
		u = keepProfile(existing, u)
		u.CreatedAt = existing.CreatedAt
		u.UpdatedAt = now
	} else {
		u.AccountState = AccountActive
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
	return withStudentCardStatus(u), !existed, nil
}

func (s *MemoryStore) UpdateProfile(_ context.Context, u User) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.byID[u.ID]
	if !ok {
		return User{}, ErrNotFound
	}
	if existing.AccountState != AccountActive {
		return User{}, ErrAccountBlocked
	}
	existing.FirstName = u.FirstName
	existing.LastName = u.LastName
	existing.Linkedin = u.Linkedin
	if !existing.YTULinked {
		existing.University = u.University
		existing.Faculty = u.Faculty
		existing.Department = u.Department
	}
	existing.Phone = u.Phone
	existing.StudentCardUID = u.StudentCardUID
	existing.ProfilePictureID = u.ProfilePictureID
	existing.ProfilePictureURL = u.ProfilePictureURL
	existing.UpdatedAt = time.Now().UTC()
	s.byID[u.ID] = existing
	return withStudentCardStatus(existing), nil
}

func (s *MemoryStore) SetYTUProfile(_ context.Context, id uuid.UUID, p ytu.Profile) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.byID[id]
	if !ok {
		return User{}, ErrNotFound
	}
	if existing.AccountState != AccountActive {
		return User{}, ErrAccountBlocked
	}
	existing.University = p.University
	existing.Faculty = p.Faculty
	existing.Department = p.Department
	existing.YTULinked = true
	existing.UpdatedAt = time.Now().UTC()
	s.byID[id] = existing
	return withStudentCardStatus(existing), nil
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
		if u.AccountState != AccountActive {
			continue
		}
		if userMatches(u, needle) {
			out = append(out, withStudentCardStatus(u))
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
		if u.AccountState != AccountActive {
			continue
		}
		if strings.ToLower(u.Email) == want {
			out = append(out, withStudentCardStatus(u))
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
		if u.AccountState == AccountActive && u.StudentCardUID == uid {
			return withStudentCardStatus(u), nil
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
	if u.AccountState != AccountActive {
		return User{}, ErrAccountBlocked
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
	return withStudentCardStatus(u), nil
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

func (s *MemoryStore) RequestDeletion(_ context.Context, id uuid.UUID, requestedBy *uuid.UUID) (DeletionRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.deletionRequests[id]; ok {
		return existing, nil
	}
	u, ok := s.byID[id]
	if !ok {
		return DeletionRequest{}, ErrNotFound
	}
	if requestedBy != nil {
		if *requestedBy != id {
			actor, exists := s.byID[*requestedBy]
			_, marked := s.deletionRequests[*requestedBy]
			if !exists || actor.AccountState != AccountActive || marked {
				return DeletionRequest{}, ErrAccountBlocked
			}
		}
	}
	now := time.Now().UTC()
	u.AccountState = AccountDeletionPending
	u.DeletionRequestedAt = &now
	u.UpdatedAt = now
	s.byID[id] = u
	request := DeletionRequest{
		ID: uuid.New(), SubjectID: id, RequestedBy: requestedBy,
		Status: DeletionRequestPending, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
	}
	s.deletionRequests[id] = request
	return request, nil
}

func (s *MemoryStore) RequestSelfDeletion(
	_ context.Context,
	id uuid.UUID,
	intake SelfDeletionIntake,
) (SelfDeletionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.deletionRequests[id]; ok {
		if record, exists := s.selfDeletionIntakes[existing.ID]; exists {
			if subtle.ConstantTimeCompare(record.IdempotencyHash[:], intake.IdempotencyHash[:]) != 1 {
				return SelfDeletionRecord{}, ErrSelfDeletionIdempotencyConflict
			}
			record.Request = existing
			record.HasCompletedStep = len(s.deletionSteps[existing.ID]) > 0
			return record, nil
		}
		record := SelfDeletionRecord{
			Request: existing, SelfDeletionIntake: intake,
			HasCompletedStep: len(s.deletionSteps[existing.ID]) > 0,
		}
		s.selfDeletionIntakes[existing.ID] = record
		return record, nil
	}

	now := intake.CreatedAt.UTC()
	u, ok := s.byID[id]
	if !ok {
		u = User{ID: id, AccountState: AccountActive, CreatedAt: now, UpdatedAt: now}
	}
	if u.AccountState != AccountActive {
		return SelfDeletionRecord{}, ErrAccountBlocked
	}
	u.AccountState = AccountDeletionPending
	u.DeletionRequestedAt = &now
	u.UpdatedAt = now
	s.byID[id] = u
	request := DeletionRequest{
		ID: uuid.New(), SubjectID: id, Status: DeletionRequestPending,
		NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
	}
	s.deletionRequests[id] = request
	record := SelfDeletionRecord{
		Request: request, SelfDeletionIntake: intake,
	}
	s.selfDeletionIntakes[request.ID] = record
	return record, nil
}

func (s *MemoryStore) SelfDeletionByReceiptLookup(_ context.Context, receiptLookupHash [32]byte) (SelfDeletionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for requestID, intake := range s.selfDeletionIntakes {
		if subtle.ConstantTimeCompare(intake.ReceiptLookupHash[:], receiptLookupHash[:]) != 1 {
			continue
		}
		for _, request := range s.deletionRequests {
			if request.ID != requestID {
				continue
			}
			intake.Request = request
			intake.HasCompletedStep = len(s.deletionSteps[requestID]) > 0
			return intake, nil
		}
	}
	return SelfDeletionRecord{}, ErrNotFound
}

func (s *MemoryStore) RetrySelfDeletion(_ context.Context, receiptLookupHash [32]byte, now time.Time) (SelfDeletionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for requestID, intake := range s.selfDeletionIntakes {
		if subtle.ConstantTimeCompare(intake.ReceiptLookupHash[:], receiptLookupHash[:]) != 1 {
			continue
		}
		if intake.ReceiptRevokedAt != nil || !now.Before(intake.ReceiptExpiresAt) {
			return SelfDeletionRecord{}, ErrNotFound
		}
		for subjectID, request := range s.deletionRequests {
			if request.ID != requestID {
				continue
			}
			if request.Status == DeletionRequestManualIntervention {
				request.Status = DeletionRequestPending
				request.AttemptCount = 0
				request.NextAttemptAt = now
				request.LeaseUntil = nil
				request.LeaseToken = nil
				request.LastErrorCode = ""
				request.UpdatedAt = now
				s.deletionRequests[subjectID] = request
			}
			intake.Request = request
			intake.HasCompletedStep = len(s.deletionSteps[requestID]) > 0
			return intake, nil
		}
	}
	return SelfDeletionRecord{}, ErrNotFound
}

func (s *MemoryStore) RevokeSelfDeletionReceipt(_ context.Context, requestID uuid.UUID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	intake, ok := s.selfDeletionIntakes[requestID]
	if !ok {
		return ErrNotFound
	}
	if intake.ReceiptRevokedAt == nil {
		intake.ReceiptRevokedAt = &at
		s.selfDeletionIntakes[requestID] = intake
	}
	return nil
}

func (s *MemoryStore) AnonymizeAccount(_ context.Context, id uuid.UUID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byID[id]
	if !ok {
		return ErrNotFound
	}
	if u.AccountState == AccountAnonymized {
		return nil
	}
	if u.AccountState != AccountDeletionPending {
		return ErrInvalid
	}
	request, requested := s.deletionRequests[id]
	if requested && request.ProfileMediaID == nil && u.ProfilePictureID != nil {
		mediaID := *u.ProfilePictureID
		request.ProfileMediaID = &mediaID
		request.UpdatedAt = at
		s.deletionRequests[id] = request
	}
	u.Email = ""
	u.FirstName = ""
	u.LastName = ""
	u.Username = ""
	u.SchoolEmail = ""
	u.SkyNumber = ""
	u.StudentCardUID = ""
	u.Linkedin = ""
	u.University = ""
	u.Faculty = ""
	u.Department = ""
	u.YTULinked = false
	u.Phone = ""
	u.ProfilePictureID = nil
	u.ProfilePictureURL = ""
	u.AccountState = AccountAnonymized
	u.AnonymizedAt = &at
	u.UpdatedAt = at
	s.byID[id] = u
	for subjectID, other := range s.deletionRequests {
		if other.RequestedBy == nil || *other.RequestedBy != id {
			continue
		}
		other.RequestedBy = nil
		other.UpdatedAt = at
		s.deletionRequests[subjectID] = other
	}
	return nil
}

func (s *MemoryStore) DeletionRequest(_ context.Context, subjectID uuid.UUID) (DeletionRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	request, ok := s.deletionRequests[subjectID]
	if !ok {
		return DeletionRequest{}, ErrNotFound
	}
	return request, nil
}

func (s *MemoryStore) ClaimDeletionRequest(_ context.Context, now time.Time, lease time.Duration) (DeletionRequest, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for subjectID, request := range s.deletionRequests {
		if request.PlatformBlockedAt == nil {
			continue
		}
		claimable := request.Status == DeletionRequestPending ||
			(request.Status == DeletionRequestProcessing && request.LeaseUntil != nil && !request.LeaseUntil.After(now))
		if !claimable || request.NextAttemptAt.After(now) {
			continue
		}
		leaseUntil := now.Add(lease)
		request.Status = DeletionRequestProcessing
		request.AttemptCount++
		request.LeaseUntil = &leaseUntil
		leaseToken := uuid.New()
		request.LeaseToken = &leaseToken
		request.UpdatedAt = now
		s.deletionRequests[subjectID] = request
		return request, true, nil
	}
	return DeletionRequest{}, false, nil
}

func (s *MemoryStore) MarkDeletionPlatformBlocked(_ context.Context, requestID uuid.UUID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for subjectID, request := range s.deletionRequests {
		if request.ID != requestID {
			continue
		}
		if request.PlatformBlockedAt == nil {
			blockedAt := at
			request.PlatformBlockedAt = &blockedAt
			request.UpdatedAt = at
			s.deletionRequests[subjectID] = request
		}
		return nil
	}
	return ErrNotFound
}

func (s *MemoryStore) UnprojectedDeletionRequests(_ context.Context, limit int) ([]DeletionRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]DeletionRequest, 0)
	for _, request := range s.deletionRequests {
		if request.PlatformBlockedAt != nil {
			continue
		}
		out = append(out, request)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *MemoryStore) DeletionRequestsPage(_ context.Context, after uuid.UUID, limit int) ([]DeletionRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		limit = 250
	}
	out := make([]DeletionRequest, 0, len(s.deletionRequests))
	for _, request := range s.deletionRequests {
		if after != uuid.Nil && strings.Compare(request.ID.String(), after.String()) <= 0 {
			continue
		}
		out = append(out, request)
	}
	slices.SortFunc(out, func(a, b DeletionRequest) int {
		return strings.Compare(a.ID.String(), b.ID.String())
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *MemoryStore) CompletedDeletionSteps(_ context.Context, requestID, leaseToken uuid.UUID) (map[DeletionStep]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.holdsLeaseLocked(requestID, leaseToken) {
		return nil, ErrLeaseLost
	}
	out := make(map[DeletionStep]bool)
	for step := range s.deletionSteps[requestID] {
		out[step] = true
	}
	return out, nil
}

func (s *MemoryStore) CompleteDeletionStep(_ context.Context, requestID, leaseToken uuid.UUID, step DeletionStep, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.holdsLeaseLocked(requestID, leaseToken) {
		return ErrLeaseLost
	}
	if s.deletionSteps[requestID] == nil {
		s.deletionSteps[requestID] = make(map[DeletionStep]time.Time)
	}
	if _, exists := s.deletionSteps[requestID][step]; !exists {
		s.deletionSteps[requestID][step] = at
	}
	if step == DeletionStepEraseProfile {
		for subjectID, request := range s.deletionRequests {
			if request.ID == requestID {
				request.ProfileMediaID = nil
				request.UpdatedAt = at
				s.deletionRequests[subjectID] = request
				break
			}
		}
	}
	return nil
}

func (s *MemoryStore) RetryDeletionRequest(_ context.Context, requestID, leaseToken uuid.UUID, next time.Time, code string, manual, refundAttempt bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for subjectID, request := range s.deletionRequests {
		if request.ID != requestID {
			continue
		}
		if request.Status != DeletionRequestProcessing || request.LeaseToken == nil || *request.LeaseToken != leaseToken {
			return ErrLeaseLost
		}
		request.Status = DeletionRequestPending
		if manual {
			request.Status = DeletionRequestManualIntervention
		}
		request.NextAttemptAt = next
		request.LeaseUntil = nil
		request.LeaseToken = nil
		request.LastErrorCode = code
		request.UpdatedAt = next
		if refundAttempt && request.AttemptCount > 0 {
			request.AttemptCount--
		}
		s.deletionRequests[subjectID] = request
		return nil
	}
	return ErrNotFound
}

func (s *MemoryStore) CompleteDeletionRequest(_ context.Context, requestID, leaseToken uuid.UUID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for subjectID, request := range s.deletionRequests {
		if request.ID != requestID {
			continue
		}
		if request.Status != DeletionRequestProcessing || request.LeaseToken == nil || *request.LeaseToken != leaseToken {
			return ErrLeaseLost
		}
		request.Status = DeletionRequestCompleted
		request.LeaseUntil = nil
		request.LeaseToken = nil
		request.LastErrorCode = ""
		request.CompletedAt = &at
		request.UpdatedAt = at
		s.deletionRequests[subjectID] = request
		return nil
	}
	return ErrNotFound
}

func (s *MemoryStore) ProfileMediaForDeletion(_ context.Context, requestID uuid.UUID) (*uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, request := range s.deletionRequests {
		if request.ID != requestID {
			continue
		}
		if request.ProfileMediaID == nil {
			return nil, nil
		}
		mediaID := *request.ProfileMediaID
		return &mediaID, nil
	}
	return nil, ErrNotFound
}

func (s *MemoryStore) holdsLeaseLocked(requestID, leaseToken uuid.UUID) bool {
	for _, request := range s.deletionRequests {
		if request.ID == requestID && request.Status == DeletionRequestProcessing && request.LeaseToken != nil && *request.LeaseToken == leaseToken {
			return true
		}
	}
	return false
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

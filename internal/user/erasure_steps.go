package user

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// DeletionStepRecord is one checkpoint of the completion proof. Counts is set
// only on service erasure steps; it never holds an address, a name or the
// subject.
type DeletionStepRecord struct {
	Step        DeletionStep
	CompletedAt time.Time
	Counts      map[string]int64
}

// CompleteServiceErasureStep checkpoints a service erasure step with the
// service's counts under the lease fence. A repeated checkpoint keeps the
// first time and counts.
func (s *PostgresStore) CompleteServiceErasureStep(ctx context.Context, requestID, leaseToken uuid.UUID, step DeletionStep, at time.Time, counts map[string]int64) error {
	if counts == nil {
		counts = map[string]int64{}
	}
	raw, err := json.Marshal(counts)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		WITH fenced AS (
			UPDATE account_deletion_requests
			SET updated_at = $4
			WHERE id = $1 AND status = 'processing' AND lease_token = $2
			RETURNING id
		)
		INSERT INTO account_deletion_steps (request_id, step, completed_at, counts)
		SELECT id, $3, $4, $5::jsonb FROM fenced
		ON CONFLICT (request_id, step) DO UPDATE
		SET completed_at = account_deletion_steps.completed_at,
			counts = COALESCE(account_deletion_steps.counts, EXCLUDED.counts)
	`, requestID, leaseToken, step, at, string(raw))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// DeletionStepRecords returns a request's checkpoints, oldest first.
func (s *PostgresStore) DeletionStepRecords(ctx context.Context, requestID uuid.UUID) ([]DeletionStepRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT step, completed_at, counts
		FROM account_deletion_steps
		WHERE request_id = $1
		ORDER BY completed_at, step
	`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make([]DeletionStepRecord, 0)
	for rows.Next() {
		var record DeletionStepRecord
		var raw []byte
		if err := rows.Scan(&record.Step, &record.CompletedAt, &raw); err != nil {
			return nil, err
		}
		if raw != nil {
			if err := json.Unmarshal(raw, &record.Counts); err != nil {
				return nil, err
			}
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// OpenDeletionRequests returns every request that is not completed, including
// those waiting for manual intervention, oldest first.
func (s *PostgresStore) OpenDeletionRequests(ctx context.Context) ([]DeletionRequest, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+deletionRequestCols+`
		FROM account_deletion_requests
		WHERE status <> 'completed'
		ORDER BY created_at, id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDeletionRequests(rows)
}

func (s *MemoryStore) CompleteServiceErasureStep(_ context.Context, requestID, leaseToken uuid.UUID, step DeletionStep, at time.Time, counts map[string]int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.holdsLeaseLocked(requestID, leaseToken) {
		return ErrLeaseLost
	}
	if s.deletionSteps[requestID] == nil {
		s.deletionSteps[requestID] = make(map[DeletionStep]time.Time)
	}
	if _, exists := s.deletionSteps[requestID][step]; exists {
		return nil
	}
	s.deletionSteps[requestID][step] = at
	if s.deletionStepCounts[requestID] == nil {
		s.deletionStepCounts[requestID] = make(map[DeletionStep]map[string]int64)
	}
	stored := maps.Clone(counts)
	if stored == nil {
		stored = map[string]int64{}
	}
	s.deletionStepCounts[requestID][step] = stored
	return nil
}

func (s *MemoryStore) DeletionStepRecords(_ context.Context, requestID uuid.UUID) ([]DeletionStepRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := make([]DeletionStepRecord, 0, len(s.deletionSteps[requestID]))
	for step, at := range s.deletionSteps[requestID] {
		records = append(records, DeletionStepRecord{Step: step, CompletedAt: at, Counts: maps.Clone(s.deletionStepCounts[requestID][step])})
	}
	slices.SortFunc(records, func(a, b DeletionStepRecord) int {
		if c := a.CompletedAt.Compare(b.CompletedAt); c != 0 {
			return c
		}
		return strings.Compare(string(a.Step), string(b.Step))
	})
	return records, nil
}

func (s *MemoryStore) OpenDeletionRequests(context.Context) ([]DeletionRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	open := make([]DeletionRequest, 0)
	for _, request := range s.deletionRequests {
		if request.Status != DeletionRequestCompleted {
			open = append(open, request)
		}
	}
	slices.SortFunc(open, func(a, b DeletionRequest) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID.String(), b.ID.String())
	})
	return open, nil
}

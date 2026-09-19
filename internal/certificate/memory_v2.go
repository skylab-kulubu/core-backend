package certificate

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
)

func (s *MemoryStore) ListTemplates(context.Context) ([]Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Template, 0, len(s.templates))
	for _, item := range s.templates {
		if item.ArchivedAt == nil {
			item.PublishedVersion = s.latestVersionLocked(item.ID)
			out = append(out, item)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *MemoryStore) GetTemplate(_ context.Context, id uuid.UUID) (Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.templates[id]
	if !ok {
		return Template{}, ErrNotFound
	}
	item.PublishedVersion = s.latestVersionLocked(id)
	return item, nil
}

func (s *MemoryStore) CreateTemplate(_ context.Context, item Template) (Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if item.ID == uuid.Nil {
		item.ID = uuid.New()
	}
	if _, exists := s.templates[item.ID]; exists {
		return Template{}, ErrConflict
	}
	now := time.Now().UTC()
	item.CreatedAt, item.UpdatedAt = now, now
	s.templates[item.ID] = item
	return item, nil
}

func (s *MemoryStore) UpdateTemplate(_ context.Context, item Template) (Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.templates[item.ID]; !exists {
		return Template{}, ErrNotFound
	}
	item.UpdatedAt = time.Now().UTC()
	s.templates[item.ID] = item
	return item, nil
}

func (s *MemoryStore) CreateVersion(_ context.Context, version TemplateVersion) (TemplateVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.templates[version.TemplateID]; !exists {
		return TemplateVersion{}, ErrNotFound
	}
	if version.ID == uuid.Nil {
		version.ID = uuid.New()
	}
	for _, existing := range s.versions {
		if existing.TemplateID == version.TemplateID && existing.Version >= version.Version {
			version.Version = existing.Version + 1
		}
	}
	if version.Version == 0 {
		version.Version = 1
	}
	version.PublishedAt = time.Now().UTC()
	s.versions[version.ID] = version
	return version, nil
}

func (s *MemoryStore) LatestVersion(_ context.Context, templateID uuid.UUID) (TemplateVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	version := s.latestVersionLocked(templateID)
	if version == nil {
		return TemplateVersion{}, ErrNotFound
	}
	return *version, nil
}

func (s *MemoryStore) latestVersionLocked(templateID uuid.UUID) *TemplateVersion {
	var latest *TemplateVersion
	for _, candidate := range s.versions {
		if candidate.TemplateID != templateID || (latest != nil && latest.Version >= candidate.Version) {
			continue
		}
		copy := candidate
		latest = &copy
	}
	return latest
}

func (s *MemoryStore) GetVersion(_ context.Context, id uuid.UUID) (TemplateVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	version, ok := s.versions[id]
	if !ok {
		return TemplateVersion{}, ErrNotFound
	}
	return version, nil
}

func (s *MemoryStore) GetBinding(_ context.Context, scope, scopeKey string) (Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, ok := s.bindings[scope+"\x00"+scopeKey]
	if !ok {
		return Binding{}, ErrNotFound
	}
	return binding, nil
}

func (s *MemoryStore) ListBindings(context.Context) ([]Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Binding, 0, len(s.bindings))
	for _, binding := range s.bindings {
		out = append(out, binding)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope == out[j].Scope {
			return out[i].ScopeKey < out[j].ScopeKey
		}
		return out[i].Scope < out[j].Scope
	})
	return out, nil
}

func (s *MemoryStore) SetBinding(_ context.Context, binding Binding) (Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if binding.ID == uuid.Nil {
		binding.ID = uuid.New()
	}
	binding.UpdatedAt = time.Now().UTC()
	s.bindings[binding.Scope+"\x00"+binding.ScopeKey] = binding
	return binding, nil
}

func (s *MemoryStore) DeleteBinding(_ context.Context, scope, scopeKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := scope + "\x00" + scopeKey
	if _, ok := s.bindings[key]; !ok {
		return ErrNotFound
	}
	delete(s.bindings, key)
	return nil
}

func (s *MemoryStore) CreateBatch(_ context.Context, batch Batch, ticketIDs []uuid.UUID) (Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if batch.ID == uuid.Nil {
		batch.ID = uuid.New()
	}
	now := time.Now().UTC()
	if batch.Reason == "finalization" {
		if _, exists := s.finalizedAt[batch.EventID]; exists {
			return Batch{}, ErrConflict
		}
		s.finalizedAt[batch.EventID] = now
	}
	batch.CreatedAt = now
	batch.TotalCount = len(ticketIDs)
	batch.QueuedCount = len(ticketIDs)
	batch.Status = "queued"
	if len(ticketIDs) == 0 {
		batch.Status = "completed"
		batch.CompletedAt = &now
	}
	for _, ticketID := range ticketIDs {
		job := Job{ID: uuid.New(), BatchID: batch.ID, EventID: batch.EventID, TicketID: ticketID, Status: "queued", NextAttemptAt: now, CreatedAt: now}
		s.jobs[job.ID] = job
		batch.Jobs = append(batch.Jobs, job)
	}
	s.batches[batch.ID] = batch
	return batch, nil
}

func (s *MemoryStore) GetBatch(_ context.Context, id uuid.UUID) (Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	batch, ok := s.batches[id]
	if !ok {
		return Batch{}, ErrNotFound
	}
	batch.Jobs = nil
	for _, job := range s.jobs {
		if job.BatchID == id {
			batch.Jobs = append(batch.Jobs, job)
		}
	}
	return batch, nil
}

func (s *MemoryStore) ListBatches(_ context.Context, eventID uuid.UUID) ([]Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Batch, 0)
	for _, batch := range s.batches {
		if batch.EventID == eventID {
			batch.Jobs = nil
			out = append(out, batch)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (s *MemoryStore) ClaimJobs(_ context.Context, limit int, lease time.Duration) ([]Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		limit = 10
	}
	now := time.Now().UTC()
	out := make([]Job, 0, limit)
	for id, job := range s.jobs {
		claimable := job.Status == "queued" && !job.NextAttemptAt.After(now)
		claimable = claimable || (job.Status == "running" && job.LeaseUntil != nil && job.LeaseUntil.Before(now))
		if !claimable {
			continue
		}
		until := now.Add(lease)
		job.Status, job.LeaseUntil = "running", &until
		job.AttemptCount++
		if job.StartedAt == nil {
			job.StartedAt = &now
		}
		s.jobs[id] = job
		batch := s.batches[job.BatchID]
		batch.Status = "running"
		if batch.StartedAt == nil {
			batch.StartedAt = &now
		}
		s.batches[batch.ID] = batch
		out = append(out, job)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *MemoryStore) CompleteJob(_ context.Context, jobID, certificateID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return ErrNotFound
	}
	now := time.Now().UTC()
	job.Status, job.CertificateID, job.CompletedAt, job.LeaseUntil = "issued", &certificateID, &now, nil
	s.jobs[jobID] = job
	s.refreshBatchLocked(job.BatchID)
	return nil
}

func (s *MemoryStore) FailJob(_ context.Context, jobID uuid.UUID, errorCode string, retryAt time.Time, terminal bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return ErrNotFound
	}
	job.Status, job.ErrorCode, job.NextAttemptAt, job.LeaseUntil = "queued", errorCode, retryAt, nil
	if terminal {
		now := time.Now().UTC()
		job.Status, job.CompletedAt = "failed", &now
	}
	s.jobs[jobID] = job
	s.refreshBatchLocked(job.BatchID)
	return nil
}

func (s *MemoryStore) RetryBatch(_ context.Context, batchID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	now := time.Now().UTC()
	for id, job := range s.jobs {
		if job.BatchID == batchID && job.Status == "failed" {
			job.Status, job.NextAttemptAt, job.CompletedAt, job.ErrorCode = "queued", now, nil, ""
			s.jobs[id] = job
			found = true
		}
	}
	if !found {
		return ErrConflict
	}
	s.refreshBatchLocked(batchID)
	return nil
}

func (s *MemoryStore) FinalizeAttendance(_ context.Context, eventID, _ uuid.UUID) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	s.finalizedAt[eventID] = now
	return now, nil
}

func (s *MemoryStore) AttendanceFinalizedAt(_ context.Context, eventID uuid.UUID) (*time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.finalizedAt[eventID]
	if !ok {
		return nil, nil
	}
	return &value, nil
}

func (s *MemoryStore) JobCounts(_ context.Context, eventID uuid.UUID) (queued, failed int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, job := range s.jobs {
		if job.EventID != eventID {
			continue
		}
		if job.Status == "queued" || job.Status == "running" {
			queued++
		}
		if job.Status == "failed" {
			failed++
		}
	}
	return queued, failed, nil
}

func (s *MemoryStore) refreshBatchLocked(batchID uuid.UUID) {
	batch, ok := s.batches[batchID]
	if !ok {
		return
	}
	batch.QueuedCount, batch.IssuedCount, batch.FailedCount = 0, 0, 0
	for _, job := range s.jobs {
		if job.BatchID != batchID {
			continue
		}
		switch job.Status {
		case "queued", "running":
			batch.QueuedCount++
		case "issued":
			batch.IssuedCount++
		case "failed":
			batch.FailedCount++
		}
	}
	if batch.QueuedCount > 0 {
		batch.Status = "running"
		batch.CompletedAt = nil
	} else {
		now := time.Now().UTC()
		batch.CompletedAt = &now
		switch {
		case batch.IssuedCount == batch.TotalCount:
			batch.Status = "completed"
		case batch.FailedCount == batch.TotalCount:
			batch.Status = "failed"
		default:
			batch.Status = "partial"
		}
	}
	s.batches[batchID] = batch
}

package certificate

import "time"

// ExpireJobLeases ends every running job's lease, as if its owner had held the
// batch past the lease; the next claim takes those jobs again.
func (s *MemoryStore) ExpireJobLeases() {
	s.mu.Lock()
	defer s.mu.Unlock()
	past := time.Now().UTC().Add(-time.Second)
	for id, job := range s.jobs {
		if job.Status == "running" {
			job.LeaseUntil = &past
			s.jobs[id] = job
		}
	}
}

// AgeMails moves every certificate mail's queueing back by d.
func (s *MemoryStore) AgeMails(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range s.mails {
		row.createdAt = row.createdAt.Add(-d)
		row.CreatedAt = row.createdAt
	}
}

// MakeMailsDue makes every queued certificate mail due now, as if its
// backoff had passed.
func (s *MemoryStore) MakeMailsDue() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range s.mails {
		row.nextAttempt = time.Time{}
	}
}

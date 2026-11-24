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

// AgeMails moves every tried certificate mail's first try back by d.
func (s *MemoryStore) AgeMails(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range s.mails {
		if !row.FirstTriedAt.IsZero() {
			row.FirstTriedAt = row.FirstTriedAt.Add(-d)
		}
	}
}

// AgeResendAll moves every Event's last "Herkese yeniden gönder" back by d.
func (s *MemoryStore) AgeResendAll(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, at := range s.resendAllAt {
		s.resendAllAt[id] = at.Add(-d)
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

// ExpireMailLeases ends every claimed mail's lease, as if its holder had
// held the batch too long.
func (s *MemoryStore) ExpireMailLeases() {
	s.mu.Lock()
	defer s.mu.Unlock()
	past := time.Now().UTC().Add(-time.Second)
	for _, row := range s.mails {
		if row.claimedUntil != nil {
			row.claimedUntil = &past
		}
	}
}

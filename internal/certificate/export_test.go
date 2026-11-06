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

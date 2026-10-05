package consent

// EmailHMAC is the proof key of a normalized address, for tests that seed
// rows in bulk.
func (s *Service) EmailHMAC(email string) []byte { return s.config.emailHMAC(email) }

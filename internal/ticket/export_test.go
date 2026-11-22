package ticket

// DrawCodes makes s draw the codes given, in turn, before fresh ones: a test
// forces a code another Ticket holds.
func DrawCodes(s *PostgresStore, codes ...string) {
	s.newCode = func() (string, error) {
		if len(codes) == 0 {
			return NewCode()
		}
		next := codes[0]
		codes = codes[1:]
		return next, nil
	}
}

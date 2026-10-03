package githubactivity

// WaitRefresh waits for the read of GitHub in flight, if any.
func (s *Service) WaitRefresh() { s.waitRefresh() }

// RepoHash is how a repository is named in logs.
func RepoHash(org, name string) string { return repoHash(org, name) }

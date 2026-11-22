package clientip

import "time"

// SetRefreshTimings sets Watch's tick and the shortest gap between lookups,
// and returns what puts them back. Not for parallel tests.
func SetRefreshTimings(every, gap time.Duration) func() {
	oldEvery, oldGap := refreshEvery, refreshGap
	refreshEvery, refreshGap = every, gap
	return func() { refreshEvery, refreshGap = oldEvery, oldGap }
}

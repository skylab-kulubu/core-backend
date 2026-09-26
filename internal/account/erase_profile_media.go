package account

import "time"

// partlyErased is an erase_profile_media pass that erased some of its Media
// but not all (a person with many files meets the step timeout). It made
// progress (Progressed), so the worker's deferred retry gives its attempt
// back even past the horizon, and the next pass goes on with what is left.
// Every such pass leaves fewer Media, and a pass that erases none returns an
// ordinary error, so the passes end.
type partlyErased struct {
	err error
	at  time.Time
}

func (e partlyErased) Error() string      { return e.err.Error() }
func (e partlyErased) Unwrap() error      { return e.err }
func (e partlyErased) RetryAt() time.Time { return e.at }
func (e partlyErased) Progressed() bool   { return true }

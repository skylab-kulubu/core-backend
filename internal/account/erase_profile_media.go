package account

import "time"

// partlyErased is an erase_profile_media pass that erased some of its Media
// but not all (a person with many files meets the step timeout). It made
// progress, so the worker's deferred retry (RetryAt) gives its attempt back
// and the next pass goes on with what is left.
type partlyErased struct {
	err error
	at  time.Time
}

func (e partlyErased) Error() string      { return e.err.Error() }
func (e partlyErased) Unwrap() error      { return e.err }
func (e partlyErased) RetryAt() time.Time { return e.at }

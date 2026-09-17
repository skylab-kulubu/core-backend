package event

import "time"

type Current struct {
	Session    *Session  `json:"session,omitempty"`
	Candidates []Session `json:"candidates"`
	ClockMatch bool      `json:"clockMatch"`
}

func ResolveCurrent(sessions []Session, at time.Time) Current {
	overlapping := make([]Session, 0)
	for _, sess := range sessions {
		if sess.StartTime == nil || sess.EndTime == nil {
			continue
		}
		if !at.Before(*sess.StartTime) && at.Before(*sess.EndTime) {
			overlapping = append(overlapping, sess)
		}
	}
	switch len(overlapping) {
	case 1:
		match := overlapping[0]
		return Current{Session: &match, Candidates: overlapping, ClockMatch: true}
	case 0:
		candidates := append([]Session(nil), sessions...)
		return Current{Candidates: candidates, ClockMatch: false}
	default:
		return Current{Candidates: overlapping, ClockMatch: false}
	}
}

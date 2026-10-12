package teamcode

import (
	"context"
	"strings"
	"sync"
)

// The reasons a code is retired (retired_team_codes.reason).
const (
	// ReasonRenamed: the Group that held the code was renamed.
	ReasonRenamed = "renamed"
)

// Ledger is what core knows of codes outside Keycloak, and the lock that
// makes a name's check and the write it allows one step: two creates or
// renames to the same name, on one core or two, cannot both pass the check.
type Ledger interface {
	// Locked runs write while no other Group create or rename runs, handing
	// it the Records as seen inside the lock. What write retires stays
	// retired only if write returns nil.
	Locked(ctx context.Context, write func(ctx context.Context, records Records) error) error
}

// Records are the codes core's own records hold.
type Records interface {
	// Held reports whether an Owner team record of core (an Event, a
	// certificate, a certificate template or its Owner team binding) or a
	// retired code holds code, compared without case.
	Held(ctx context.Context, code string) (bool, error)
	// Retire records code as given up by the Group groupID for reason: no
	// new Group gets it again. groupID is the Keycloak id, recorded only
	// when it is a UUID. Retiring a retired code again changes nothing.
	Retire(ctx context.Context, code, groupID, reason string) error
}

// MemoryLedger is a Ledger in this process only: a lock no other core sees
// and the codes Hold gave it. For tests and for a service without a
// database.
type MemoryLedger struct {
	lock sync.Mutex

	mu      sync.Mutex
	held    map[string]bool
	retired map[string]string
}

func NewMemoryLedger() *MemoryLedger {
	return &MemoryLedger{held: map[string]bool{}, retired: map[string]string{}}
}

// Hold makes codes held, as an Owner team record would.
func (l *MemoryLedger) Hold(codes ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, code := range codes {
		l.held[strings.ToLower(code)] = true
	}
}

// Retired are the retired codes and their reasons.
func (l *MemoryLedger) Retired() map[string]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]string, len(l.retired))
	for code, reason := range l.retired {
		out[code] = reason
	}
	return out
}

func (l *MemoryLedger) Locked(ctx context.Context, write func(ctx context.Context, records Records) error) error {
	l.lock.Lock()
	defer l.lock.Unlock()
	tx := &memoryRecords{ledger: l, retired: map[string]string{}}
	if err := write(ctx, tx); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for code, reason := range tx.retired {
		if _, ok := l.retired[code]; !ok {
			l.retired[code] = reason
		}
	}
	return nil
}

// memoryRecords are a MemoryLedger's Records inside its lock: what they
// retire is kept once the write succeeds.
type memoryRecords struct {
	ledger  *MemoryLedger
	retired map[string]string
}

func (r *memoryRecords) Held(_ context.Context, code string) (bool, error) {
	r.ledger.mu.Lock()
	defer r.ledger.mu.Unlock()
	if r.ledger.held[strings.ToLower(code)] {
		return true, nil
	}
	for retired := range r.ledger.retired {
		if strings.EqualFold(retired, code) {
			return true, nil
		}
	}
	for retired := range r.retired {
		if strings.EqualFold(retired, code) {
			return true, nil
		}
	}
	return false, nil
}

func (r *memoryRecords) Retire(_ context.Context, code, _, reason string) error {
	r.retired[code] = reason
	return nil
}

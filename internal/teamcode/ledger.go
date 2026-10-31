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
	// ReasonDeleted: an empty Group was deleted. Its code is never reused.
	ReasonDeleted = "deleted"
	// ReasonSeeded: core's records or another system (the CMS, Forms, the
	// sites) name the code and no Group holds it; the team structure
	// report wrote it (core-backend team-structure-report -seed-retired).
	ReasonSeeded = "seeded"
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
	// References are the kinds of Owner team record of core that hold
	// code (RecordEvent, …, in that order), compared without case; none is
	// nil. Retired codes are left out: deleting a Group may be retried
	// after its code was durably retired. A kind, never a record's id.
	References(ctx context.Context, code string) ([]string, error)
	// Retire records code as given up by the Group groupID for reason: no
	// new Group gets it again. groupID is the Keycloak id, recorded only
	// when it is a UUID. Retiring a retired code again changes nothing.
	Retire(ctx context.Context, code, groupID, reason string) error
}

// The kinds of Owner team record (Records.References), as a refusal names
// them (code_in_use): which records still name a code, never which one.
const (
	RecordEvent               = "event"
	RecordCertificate         = "certificate"
	RecordCertificateTemplate = "certificate_template"
	RecordTemplateBinding     = "certificate_template_binding"
)

// recordKinds are the kinds in the order References answers them.
var recordKinds = []string{RecordEvent, RecordCertificate, RecordCertificateTemplate, RecordTemplateBinding}

// MemoryLedger is a Ledger in this process only: a lock no other core sees
// and the codes Hold gave it. For tests and for a service without a
// database.
type MemoryLedger struct {
	lock sync.Mutex

	mu sync.Mutex
	// held are the kinds of record that hold each code, by its lower case.
	held    map[string]map[string]bool
	retired map[string]string
}

func NewMemoryLedger() *MemoryLedger {
	return &MemoryLedger{held: map[string]map[string]bool{}, retired: map[string]string{}}
}

// Hold makes codes held, as an Event's Owner team would.
func (l *MemoryLedger) Hold(codes ...string) { l.HoldAs(RecordEvent, codes...) }

// HoldAs makes codes held by a record of kind (RecordEvent, …).
func (l *MemoryLedger) HoldAs(kind string, codes ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, code := range codes {
		key := strings.ToLower(code)
		if l.held[key] == nil {
			l.held[key] = map[string]bool{}
		}
		l.held[key][kind] = true
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
	if len(r.ledger.held[strings.ToLower(code)]) > 0 {
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

func (r *memoryRecords) References(_ context.Context, code string) ([]string, error) {
	r.ledger.mu.Lock()
	defer r.ledger.mu.Unlock()
	var out []string
	for _, kind := range recordKinds {
		if r.ledger.held[strings.ToLower(code)][kind] {
			out = append(out, kind)
		}
	}
	return out, nil
}

func (r *memoryRecords) Retire(_ context.Context, code, _, reason string) error {
	r.retired[code] = reason
	return nil
}

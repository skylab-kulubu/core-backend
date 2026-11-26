package usersync

import (
	"context"
	"errors"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/identity"
)

type countingLinker struct{ passes int }

func (l *countingLinker) LinkGuests(context.Context) { l.passes++ }

// The guest-account link (docs/guest-account-links.md) is the last step of
// every pass that read Keycloak, under the pass's lock: not of a pass
// another replica holds the lock of, nor of one Keycloak failed.
func TestPassEndsWithTheGuestLink(t *testing.T) {
	t.Parallel()
	rows := newFakeRows()
	dir := &fakeDir{people: []identity.Person{person(nil)}}
	linker := &countingLinker{}
	w := worker(dir, rows, nil)
	w.SetGuestLinker(linker)
	if _, err := w.Pass(context.Background()); err != nil || linker.passes != 1 {
		t.Fatalf("pass: %v, linked %d times", err, linker.passes)
	}
	rows.locked = true
	if report, err := w.Pass(context.Background()); err != nil || !report.Locked || linker.passes != 1 {
		t.Fatalf("locked pass: %+v %v, linked %d times", report, err, linker.passes)
	}
	rows.locked = false
	dir.err = errors.New("keycloak 503")
	if _, err := w.Pass(context.Background()); err == nil || linker.passes != 1 {
		t.Fatalf("failed pass: %v, linked %d times", err, linker.passes)
	}
}

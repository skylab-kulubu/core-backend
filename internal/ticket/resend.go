package ticket

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// Sending a Ticket's mail again and renewing its link from the panel
// (ADR 0082; docs/ticket-resend.md).
var (
	// ErrResendLimited is a Ticket sent ResendLimit times in the last
	// ResendWindow.
	ErrResendLimited = errors.New("ticket: resend limited")
	// ErrNotificationOff is TICKET_CREATED_NOTIFICATION off: no mail goes.
	ErrNotificationOff = errors.New("ticket: ticket notification off")
	// ErrHolderErased is a Ticket whose holder was erased: there is nobody
	// to send it to, nor a link to renew.
	ErrHolderErased = errors.New("ticket: holder erased")
	// ErrEventEnded is an Event that is over: its Tickets are not sent again.
	ErrEventEnded = errors.New("ticket: event ended")
)

const (
	// ResendLimit is how many times a Ticket's mail may go again in
	// ResendWindow.
	ResendLimit  = 3
	ResendWindow = time.Hour
	// maxRequestKey bounds the client's Idempotency-Key.
	maxRequestKey = 128
)

// Resend is what sending a Ticket's mail again did.
type Resend struct {
	// N is the Ticket's resend count: its Notification's key ends :r<N>.
	N int `json:"resend"`
	// Replayed: a request with the same Idempotency-Key was answered
	// before; nothing new was written.
	Replayed bool `json:"replayed"`
	// Queued: the mail is queued.
	Queued bool `json:"queued"`
}

// ResendStore keeps the resends (PostgresStore).
type ResendStore interface {
	NotificationsOn() bool
	QueueResend(ctx context.Context, id uuid.UUID, requestHash string, limit int, window time.Duration) (Resend, error)
	RenewAccessKey(ctx context.Context, id uuid.UUID, notify bool, requestHash string, limit int, window time.Duration) (Ticket, Resend, error)
}

// RequestHash is what is kept of a client's Idempotency-Key: its SHA-256,
// "" for none; ErrInvalid for one longer than 128 characters.
func RequestHash(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", nil
	}
	if len(key) > maxRequestKey {
		return "", ErrInvalid
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]), nil
}

// managedTicket is the Ticket id names and its Event, for who may give
// out the Event's Tickets: who may assign them (ticket:manage, the owning
// team's Leaders; the rule of Apply-for-other). A draft p may not see is
// ErrNotFound; a Ticket whose holder was erased ErrHolderErased.
func (s *service) managedTicket(ctx context.Context, p authz.Principal, id uuid.UUID) (Ticket, event.Event, error) {
	t, err := s.tickets.Get(ctx, id)
	if err != nil {
		return Ticket{}, event.Event{}, err
	}
	ev, err := s.eventOf(ctx, t.EventID)
	if err != nil {
		return Ticket{}, event.Event{}, err
	}
	if err := s.seen(&p, ev); err != nil {
		return Ticket{}, event.Event{}, err
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket, OwnerTeam: ev.OwnerTeam}, authz.Assign) {
		return Ticket{}, event.Event{}, ErrForbidden
	}
	if !held(t) {
		return Ticket{}, event.Event{}, ErrHolderErased
	}
	return t, ev, nil
}

// mailable is nil when a Ticket of ev may be sent again: a published Event
// that is not over, while the Notifications are on.
func (s *service) mailable(ev event.Event, store ResendStore) error {
	switch {
	case ev.Status == event.StatusCancelled:
		return ErrEventCancelled
	case ev.Status != event.StatusPublished:
		return ErrEventNotPublished
	case ev.EndedAt(time.Now()):
		return ErrEventEnded
	case store == nil || !store.NotificationsOn():
		return ErrNotificationOff
	}
	return nil
}

// ResendTicket queues the Ticket's "Biletin hazır" mail again, under a key
// of its own (…:r<n>).
func (s *service) ResendTicket(ctx context.Context, p authz.Principal, id uuid.UUID, requestKey string) (Resend, error) {
	hash, err := RequestHash(requestKey)
	if err != nil {
		return Resend{}, err
	}
	_, ev, err := s.managedTicket(ctx, p, id)
	if err != nil {
		return Resend{}, err
	}
	store, _ := s.tickets.(ResendStore)
	if err := s.mailable(ev, store); err != nil {
		return Resend{}, err
	}
	return store.QueueResend(ctx, id, hash, ResendLimit, ResendWindow)
}

// RenewTicketLink gives the Ticket a new access key and the next QR version,
// and with notify queues its mail with the new link. Without notify (the
// panel's notify=false) only the link is renewed, on any Event: an operator
// who ends a leaked link at once.
func (s *service) RenewTicketLink(ctx context.Context, p authz.Principal, id uuid.UUID, notify bool, requestKey string) (Ticket, Resend, error) {
	hash, err := RequestHash(requestKey)
	if err != nil {
		return Ticket{}, Resend{}, err
	}
	_, ev, err := s.managedTicket(ctx, p, id)
	if err != nil {
		return Ticket{}, Resend{}, err
	}
	store, ok := s.tickets.(ResendStore)
	if !ok {
		return Ticket{}, Resend{}, ErrNotificationOff
	}
	if notify {
		if err := s.mailable(ev, store); err != nil {
			return Ticket{}, Resend{}, err
		}
	}
	renewed, sent, err := store.RenewAccessKey(ctx, id, notify, hash, ResendLimit, ResendWindow)
	if err != nil {
		return Ticket{}, Resend{}, err
	}
	return s.withEvent(ctx, renewed), sent, nil
}

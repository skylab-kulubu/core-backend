package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/core-backend/internal/events"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// keycloakQueue is the queue of Keycloak's events for core (ADR-0071 §5).
const keycloakQueue = "core.keycloak"

// keycloakHandling is what core does with Keycloak's events. Every field may
// be nil: that part is off.
type keycloakHandling struct {
	// realm is core's realm as the provider's routing keys spell it
	// (keycloakRoutingRealm); an event of another realm is unrelated.
	realm string
	// wakeUserSync wakes the user sync (nil when it is off).
	wakeUserSync func()
	// people reads a person from Keycloak: identity.ErrNotFound when
	// Keycloak no longer has them.
	people interface {
		GetUser(ctx context.Context, id uuid.UUID) (identity.Person, error)
	}
	// requested says whether the person has an erasure request: the
	// erasure saga's to delete (docs/account-lifecycle.md).
	requested func(ctx context.Context, tx pgx.Tx, person uuid.UUID) (bool, error)
	// erase deletes a person's communication preferences in tx.
	erase func(ctx context.Context, tx pgx.Tx, person uuid.UUID) (int64, error)
	// wakeTeams wakes the communication preference cleanup's deleted teams
	// part.
	wakeTeams func()
	logf      func(string, ...any)

	outsideErasure atomic.Int64
}

// keycloakEvents is the core.keycloak subscription. Each event only wakes
// something; what changes is decided on a fresh read of Keycloak, and the
// periodic passes catch whatever event was lost (Keycloak publishes after its
// commit, without an outbox; ADR-0071 sapma 4):
//
//   - A sign-up, a person's change of e-mail or profile, and an admin's new,
//     changed or deleted user wake the user sync (docs/user-sync.md).
//   - A deleted person (an admin's USER DELETE, the account console's
//     DELETE_ACCOUNT) whose erasure request core does not have, and whom
//     Keycloak no longer has, loses their communication preferences in the
//     event's transaction, with a warning: the deletion skipped the erasure
//     saga. A person Keycloak still has, or the saga's, keeps them.
//   - A deleted Group wakes the communication preference cleanup's deleted
//     teams part, which reads the whole Group list (a deleted Group's
//     sub-Groups get no event of their own).
//
// A deletion from the account console (off in SKY LAB's realm) is logged as
// a warning too. Any other event, a failed one, one of another realm, and
// one whose part is off are acked and not recorded.
func keycloakEvents(k *keycloakHandling) events.Subscription {
	return events.Subscription{
		Queue:  keycloakQueue,
		Decode: events.DecodeKeycloak,
		Handle: func(ctx context.Context, tx pgx.Tx, e events.Event) error {
			kc, err := events.KeycloakOf(e)
			if err != nil {
				return events.Permanent(err)
			}
			if kc.Failed || (k.realm != "" && kc.Realm != "" && kc.Realm != k.realm) {
				return events.ErrUnrelated
			}
			acted := false
			if person, ok := deletedPerson(kc); ok && k.people != nil && k.requested != nil && k.erase != nil {
				if err := k.personDeleted(ctx, tx, e, person); err != nil {
					return err
				}
				acted = true
			}
			if kc.Admin && kc.ResourceType == "GROUP" && kc.OperationType == "DELETE" && k.wakeTeams != nil {
				k.wakeTeams()
				acted = true
			}
			if wakesUserSync(kc) {
				if !kc.Admin && kc.Type == "DELETE_ACCOUNT" {
					k.logf(`{"event":"keycloak_account_deleted_by_user","level":"warn","queue":%q,"id":%q}`, e.Queue, e.ID)
				}
				if k.wakeUserSync != nil {
					k.wakeUserSync()
					acted = true
				}
			}
			if !acted {
				return events.ErrUnrelated
			}
			return nil
		},
	}
}

// personDeleted deletes the communication preferences of a person Keycloak
// said it deleted, when the erasure saga does not have them and Keycloak no
// longer has them. A Keycloak error other than "not found" is transient.
func (k *keycloakHandling) personDeleted(ctx context.Context, tx pgx.Tx, e events.Event, person uuid.UUID) error {
	requested, err := k.requested(ctx, tx, person)
	if err != nil {
		return fmt.Errorf("reading the erasure requests: %w", err)
	}
	if requested {
		return nil
	}
	switch _, err := k.people.GetUser(ctx, person); {
	case err == nil:
		return nil
	case !errors.Is(err, identity.ErrNotFound):
		return fmt.Errorf("reading the deleted person from Keycloak: %w", err)
	}
	removed, err := k.erase(ctx, tx, person)
	if err != nil {
		return fmt.Errorf("erasing the deleted person's communication preferences: %w", err)
	}
	k.outsideErasure.Add(1)
	k.logf(`{"event":"keycloak_user_deleted_outside_erasure","level":"warn","queue":%q,"id":%q,"subject":%q,"preferences_removed":%d}`,
		e.Queue, e.ID, person.String(), removed)
	return nil
}

// String is the startup line's account of what core.keycloak's events do.
func (k *keycloakHandling) String() string {
	state := func(on bool) string {
		if on {
			return "on"
		}
		return "off"
	}
	return fmt.Sprintf("(realm %q): user sync wake %s, deleted person cleanup %s, deleted team cleanup wake %s",
		k.realm, state(k.wakeUserSync != nil), state(k.people != nil && k.requested != nil && k.erase != nil), state(k.wakeTeams != nil))
}

// prometheus is the count of people deleted in Keycloak outside the erasure
// saga that an event found.
func (k *keycloakHandling) prometheus() string {
	return fmt.Sprintf("# TYPE skylab_keycloak_users_deleted_outside_erasure_total counter\nskylab_keycloak_users_deleted_outside_erasure_total %d\n", k.outsideErasure.Load())
}

// deletedPerson is the person a deletion event names: an admin's USER
// DELETE by its resource path, a DELETE_ACCOUNT by its user id.
func deletedPerson(kc events.KeycloakEvent) (uuid.UUID, bool) {
	var raw string
	switch {
	case kc.Admin && kc.ResourceType == "USER" && kc.OperationType == "DELETE":
		id, ok := strings.CutPrefix(kc.ResourcePath, "users/")
		if !ok {
			return uuid.UUID{}, false
		}
		raw = id
	case !kc.Admin && kc.Type == "DELETE_ACCOUNT":
		raw = kc.UserID
	default:
		return uuid.UUID{}, false
	}
	id, err := uuid.Parse(raw)
	return id, err == nil
}

// erasureRequested says whether core has an erasure request for person, in
// any state: the saga's to delete, or deleted.
func erasureRequested(ctx context.Context, tx pgx.Tx, person uuid.UUID) (bool, error) {
	var requested bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM account_deletion_requests WHERE subject_id = $1)`, person).Scan(&requested)
	return requested, err
}

var (
	routingKeyOutsideSet = regexp.MustCompile(`[^*#a-zA-Z0-9 _.-]`)
	routingKeyDot        = regexp.MustCompile(`\.`)
)

// keycloakRoutingRealm is realm as the provider writes it in a routing key:
// its dots out, any character outside [*#a-zA-Z0-9 _.-] out, spaces as _.
func keycloakRoutingRealm(realm string) string {
	realm = routingKeyDot.ReplaceAllString(realm, "")
	return strings.ReplaceAll(routingKeyOutsideSet.ReplaceAllString(realm, ""), " ", "_")
}

// wakesUserSync says whether a Keycloak event changes what the user sync
// reads: an account, its e-mail or its names.
func wakesUserSync(kc events.KeycloakEvent) bool {
	if kc.Admin {
		if kc.ResourceType != "USER" {
			return false
		}
		switch kc.OperationType {
		case "CREATE", "UPDATE", "DELETE":
			return true
		}
		return false
	}
	switch kc.Type {
	case "REGISTER", "UPDATE_EMAIL", "UPDATE_PROFILE", "DELETE_ACCOUNT":
		return true
	}
	return false
}

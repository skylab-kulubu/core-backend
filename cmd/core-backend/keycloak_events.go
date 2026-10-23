package main

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/core-backend/internal/events"
)

// keycloakQueue is the queue of Keycloak's events for core (ADR-0071 §5).
const keycloakQueue = "core.keycloak"

// keycloakEvents is the core.keycloak subscription: a sign-up, a person's
// change of e-mail or profile, and an admin's new, changed or deleted user
// wake the user sync (docs/user-sync.md), which reads Keycloak and brings
// core's row in line within seconds instead of at the next tick. The event
// itself writes nothing: the pass decides, and the pass every
// USER_SYNC_INTERVAL catches whatever event was lost. A deletion from
// Keycloak's account console (DELETE_ACCOUNT, off in SKY LAB's realm) wakes
// it too and is logged as a warning. Any other event, a failed one, and every
// event while the sync is off (wake nil) are acked and not recorded.
func keycloakEvents(wake func(), logf func(string, ...any)) events.Subscription {
	return events.Subscription{
		Queue:  keycloakQueue,
		Decode: events.DecodeKeycloak,
		Handle: func(_ context.Context, _ pgx.Tx, e events.Event) error {
			kc, err := events.KeycloakOf(e)
			if err != nil {
				return events.Permanent(err)
			}
			if kc.Failed || !wakesUserSync(kc) {
				return events.ErrUnrelated
			}
			if !kc.Admin && kc.Type == "DELETE_ACCOUNT" {
				logf(`{"event":"keycloak_account_deleted_by_user","level":"warn","queue":%q,"id":%q}`, e.Queue, e.ID)
			}
			if wake == nil {
				return events.ErrUnrelated
			}
			wake()
			return nil
		},
	}
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

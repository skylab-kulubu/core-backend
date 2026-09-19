package lifecycle

import "github.com/google/uuid"

// ActorID converts an authenticated subject into optional lifecycle metadata.
// Machine actors and legacy identities that are not UUIDs remain unattributed.
func ActorID(subject string) *uuid.UUID {
	id, err := uuid.Parse(subject)
	if err != nil {
		return nil
	}
	return &id
}

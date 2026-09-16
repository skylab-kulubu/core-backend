package authn

import (
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const LocalsIdentity = "identity"
const LocalsUser = "user"

type Identity struct {
	ID      uuid.UUID
	Profile user.Profile
	Groups  []string
}

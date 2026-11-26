package ticket

import (
	"time"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// eventResource is ev's decision of type t, its Co-organizers included
// (event.AuthzResource, docs/co-organizers.md).
func eventResource(ev event.Event, t authz.Type) authz.Resource {
	return event.AuthzResource(ev, t, time.Now())
}

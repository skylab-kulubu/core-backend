package teamaudit

import (
	"testing"

	"github.com/google/uuid"
)

// An event editor's row: the Ticket actions an Event's editor takes
// without being Privileged or a Leader (a GECEKODU member, an Event's
// Co-organizer, docs/co-organizers.md), a resend and a link renewal among
// them; never a Team's own action.
func TestEventEditorBasisActions(t *testing.T) {
	t.Parallel()
	for _, action := range []Action{ActionTicketCancel, ActionTicketRestore, ActionTicketPromote, ActionTicketImport, ActionTicketImportPreview, ActionRaffleDraw, ActionRaffleForfeit, ActionTicketResend, ActionTicketLinkRenew} {
		r := Record{
			Actor:   Actor{ID: uuid.New(), Basis: BasisEventEditor, ViaGroupID: uuid.NewString(), Client: "core-frontend"},
			Action:  action,
			GroupID: uuid.NewString(), GroupPath: "/UYELER/ARGE/WEBLAB",
			After: &State{EventID: uuid.NewString(), TicketID: uuid.NewString()},
		}
		if _, bad := r.normalize(); bad != "" {
			t.Errorf("%s: %s", action, bad)
		}
	}
	r := validRecord()
	r.Actor.Basis, r.Actor.ViaGroupID = BasisEventEditor, uuid.NewString()
	if _, bad := r.normalize(); bad != "invalid_basis" {
		t.Errorf("member.add as an event editor: %q", bad)
	}
}

package eventmail

import (
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type Recipient struct {
	ID       uuid.UUID `json:"id,omitempty"`
	Email    string    `json:"email"`
	FullName string    `json:"fullName"`
}

func ListName(ownerTeam, eventName string) string {
	return strings.TrimSpace(strings.Join([]string{strings.TrimSpace(ownerTeam), strings.TrimSpace(eventName)}, " "))
}

func RecipientsFromTickets(tickets []ticket.Ticket, users map[uuid.UUID]user.User) []Recipient {
	out := make([]Recipient, 0, len(tickets))
	seen := map[string]struct{}{}
	for _, row := range tickets {
		email, name := ticketRecipient(row, users)
		email = strings.ToLower(strings.TrimSpace(email))
		if email == "" {
			continue
		}
		if _, ok := seen[email]; ok {
			continue
		}
		seen[email] = struct{}{}
		name = strings.TrimSpace(name)
		if name == "" {
			name = email
		}
		out = append(out, Recipient{Email: email, FullName: name})
	}
	return out
}

func ticketRecipient(row ticket.Ticket, users map[uuid.UUID]user.User) (email, name string) {
	if row.TicketType == ticket.Guest {
		return row.GuestEmail, strings.TrimSpace(row.GuestFirstName + " " + row.GuestLastName)
	}
	if row.OwnerID == nil {
		return "", ""
	}
	u, ok := users[*row.OwnerID]
	if !ok {
		return "", ""
	}
	return u.Email, strings.TrimSpace(u.FirstName + " " + u.LastName)
}

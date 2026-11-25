package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/ticketnotify"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// ticketCreatedNotificationOn reads TICKET_CREATED_NOTIFICATION
// (docs/ticket-created-notification.md): unset or off is off, on is on.
func ticketCreatedNotificationOn(getenv func(string) string) (bool, error) {
	return onOff(getenv, "TICKET_CREATED_NOTIFICATION")
}

// ticketPDFAttachmentOn reads TICKET_PDF_ATTACHMENT
// (docs/ticket-created-notification.md, "The PDF ticket"): on, the Ticket's
// PDF goes with its mail; unset or off, the mail goes without it.
func ticketPDFAttachmentOn(getenv func(string) string) (bool, error) {
	return onOff(getenv, "TICKET_PDF_ATTACHMENT")
}

// certificatePDFAttachmentOn reads CERTIFICATE_PDF_ATTACHMENT
// (docs/certificate-mail.md, "PDF"): on, the certificate's PDF goes with its
// queued mail; unset or off, the mail goes without it.
func certificatePDFAttachmentOn(getenv func(string) string) (bool, error) {
	return onOff(getenv, "CERTIFICATE_PDF_ATTACHMENT")
}

// coOrganizersWriteOn reads EVENT_CO_ORGANIZERS_WRITE
// (docs/co-organizers.md): on, PUT /v1/events/{id}/organizers writes;
// unset or off, it is refused (co_organizers_disabled) and the lists stay
// as they are.
func coOrganizersWriteOn(getenv func(string) string) (bool, error) {
	return onOff(getenv, "EVENT_CO_ORGANIZERS_WRITE")
}

// ticketStatusMailOn reads TICKET_STATUS_MAIL (docs/event-capacity.md,
// docs/ticket-status-mail.md): on, the waiting list's mails go ("Bekleme
// listesindesin", "Yer açıldı") and the cancellation's ("Biletin iptal
// edildi"); unset or off, none does, and a Ticket promoted off the list
// gets the new Ticket's mail while that is on. It needs SkyNotify's templates
// (event.waitlist-joined, event.waitlist-promoted, event.ticket-cancelled)
// seeded first.
func ticketStatusMailOn(getenv func(string) string) (bool, error) {
	return onOff(getenv, "TICKET_STATUS_MAIL")
}

// onOff reads an on/off switch: unset or off is off, on is on, anything
// else is refused.
func onOff(getenv func(string) string, name string) (bool, error) {
	switch raw := strings.TrimSpace(getenv(name)); raw {
	case "", "off":
		return false, nil
	case "on":
		return true, nil
	default:
		return false, fmt.Errorf("%s must be on or off, not %q", name, raw)
	}
}

// startTicketCreatedNotification starts the worker that tells a new Ticket's
// holder (iletişim-servisi 06) until ctx is cancelled, and answers a channel
// that closes once it has stopped. Off, or without SkyNotify, a new Ticket
// queues nothing: turning it on later does not tell the Tickets written
// meanwhile. pdf, when not nil (TICKET_PDF_ATTACHMENT=on), draws each
// Ticket's PDF for its mail.
//
// statusOn (TICKET_STATUS_MAIL) adds the waiting list's mails to the same
// queue and worker: the worker runs while either is on.
func startTicketCreatedNotification(ctx context.Context, on, statusOn bool, store *ticket.PostgresStore, queue ticketnotify.Queue, events *event.PostgresStore, users user.Store, sky *mail.SkyNotify, page ticket.PageConfig, apiOrigin string, pdf ticketnotify.PDFs, logf func(string, ...any)) <-chan struct{} {
	off := make(chan struct{})
	close(off)
	store.NotifyCreated(false)
	store.NotifyStatus(false)
	events.NotifyTicketsOnPublish(false)
	events.NotifyTicketStatus(false)
	switch {
	case !on && !statusOn:
		logf("ticket notification: off (TICKET_CREATED_NOTIFICATION is not on)")
		logf("ticket status mail: off (TICKET_STATUS_MAIL is not on)")
		return off
	case sky == nil:
		logf("ticket notification: off (SkyNotify is not configured)")
		return off
	}
	svc, err := ticketnotify.NewService(ticketnotify.Config{Queue: queue, Tickets: store, Events: events, Users: users, Mail: sky, Page: page, APIOrigin: apiOrigin, PDF: pdf})
	if err != nil {
		logf("ticket notification: OFF, %v", err)
		return off
	}
	store.NotifyStatus(statusOn)
	events.NotifyTicketStatus(statusOn)
	if statusOn {
		logf("ticket status mail: on (%s, %s, %s)", mail.WaitlistJoinedType, mail.WaitlistPromotedType, mail.TicketCancelledType)
	} else {
		logf("ticket status mail: off (TICKET_STATUS_MAIL is not on)")
	}
	if !on {
		logf("ticket notification: off (TICKET_CREATED_NOTIFICATION is not on)")
		return svc.Run(ctx, logf)
	}
	store.NotifyCreated(true)
	events.NotifyTicketsOnPublish(true)
	attachment := "PDF attachment off"
	if pdf != nil {
		attachment = "PDF attachment on"
	}
	if page.URL == "" {
		logf("ticket notification: on (%s), no ticket page (%s unset), %s", mail.TicketCreatedType, ticket.PageURLEnv, attachment)
	} else {
		logf("ticket notification: on (%s), ticket page on, %s", mail.TicketCreatedType, attachment)
	}
	return svc.Run(ctx, logf)
}

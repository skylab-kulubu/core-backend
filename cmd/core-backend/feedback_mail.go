package main

import (
	"context"

	"github.com/skylab-kulubu/core-backend/internal/feedback"
	"github.com/skylab-kulubu/core-backend/internal/feedbackmail"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// startFeedbackMail starts the worker that plans and sends the feedback
// mails (docs/event-feedback.md, ticket 06) until ctx is cancelled, and
// answers a channel that closes once it has stopped. Without SkyMail it is
// off: nothing is planned, so a target ending meanwhile is planned once
// SkyMail is configured (within feedback.AutoWindow). A kind whose template
// key is empty plans nothing until a key is set, the same way.
func startFeedbackMail(ctx context.Context, queue *feedback.PostgresStore, tickets ticket.Store, users user.Store, sky *mail.SkyMail, logf func(string, ...any)) <-chan struct{} {
	off := make(chan struct{})
	close(off)
	if sky == nil {
		logf("feedback mail: off (SkyMail is not configured)")
		return off
	}
	svc, err := feedbackmail.NewService(feedbackmail.Config{Queue: queue, Tickets: tickets, Users: users, Mail: sky})
	if err != nil {
		logf("feedback mail: OFF, %v", err)
		return off
	}
	request, reminder := sky.FeedbackTemplateKey(mail.FeedbackRequest), sky.FeedbackTemplateKey(mail.FeedbackReminder)
	if request == "" {
		request = "off"
	}
	if reminder == "" {
		reminder = "off"
	}
	logf("feedback mail: on (request %s, reminder %s)", request, reminder)
	return svc.Run(ctx, logf)
}

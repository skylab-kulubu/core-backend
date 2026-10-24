package events

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	amqp "github.com/rabbitmq/amqp091-go"
)

// Outcomes of a delivery, the label of skylab_events_consumed_total.
const (
	OutcomeProcessed    = "processed"
	OutcomeDuplicate    = "duplicate"
	OutcomeUnrelated    = "unrelated"
	OutcomeDeadLettered = "dead_lettered"
)

// Reasons a delivery went to the DLQ, the label of skylab_events_dlq_total.
const (
	ReasonInvalidEnvelope  = "invalid_envelope"
	ReasonPermanent        = "permanent"
	ReasonRetriesExhausted = "retries_exhausted"
	ReasonHandlerPanic     = "handler_panic"
)

var (
	outcomes = []string{OutcomeProcessed, OutcomeDuplicate, OutcomeUnrelated, OutcomeDeadLettered}
	reasons  = []string{ReasonInvalidEnvelope, ReasonPermanent, ReasonRetriesExhausted, ReasonHandlerPanic}
)

// panicError is a handler's panic, recovered.
type panicError struct{ value any }

func (e panicError) Error() string { return fmt.Sprintf("handler panicked: %v", e.value) }

// logText is the panic as a log line carries it: a runtime error's text
// (an index out of range, a nil pointer), any other value's type only, as
// it may hold the event's data.
func (e panicError) logText() string {
	if r, ok := e.value.(runtime.Error); ok {
		return "handler panicked: " + r.Error()
	}
	return fmt.Sprintf("handler panicked with a %T", e.value)
}

// deliver decodes and handles one delivery and settles it: ack, or nack
// without requeue to the queue's dead-letter exchange. Nothing requeues at
// once (a quorum queue does not count an explicit requeue against its
// delivery limit, so it would loop). When ctx ends first the delivery is
// left unsettled: the closing channel returns it to the queue.
func (c *Consumer) deliver(ctx context.Context, s *subscription, d amqp.Delivery) {
	if d.Redelivered {
		c.metrics.redelivered(s.Queue)
	}
	event, err := s.Decode(Message{
		Body: d.Body, ContentType: d.ContentType, MessageID: d.MessageId,
		Type: d.Type, AppID: d.AppId, Headers: d.Headers,
	})
	if err != nil {
		c.logf(`{"event":"events_dead_lettered","level":"warn","queue":%q,"reason":%q,"error":%q}`, s.Queue, ReasonInvalidEnvelope, c.errorText(err))
		if err := d.Reject(false); err != nil {
			c.ackFailed(s, err)
			return
		}
		c.metrics.deadLettered(s.Queue, ReasonInvalidEnvelope)
		return
	}
	event.Queue, event.Redelivered = s.Queue, d.Redelivered

	for attempt := 0; ; attempt++ {
		duplicate, err := c.attempt(ctx, s, event)
		if err == nil || errors.Is(err, ErrUnrelated) {
			outcome := OutcomeProcessed
			switch {
			case err != nil:
				outcome = OutcomeUnrelated
			case duplicate:
				outcome = OutcomeDuplicate
			}
			if err := d.Ack(false); err != nil {
				// The work is committed; the broker delivers it again
				// and the record makes that a duplicate.
				c.ackFailed(s, err)
				return
			}
			c.metrics.consumed(s.Queue, outcome)
			return
		}
		if ctx.Err() != nil {
			return
		}
		reason := ""
		var p panicError
		switch {
		case errors.As(err, &p):
			reason = ReasonHandlerPanic
		case IsPermanent(err):
			reason = ReasonPermanent
		case attempt >= len(c.retryDelays):
			reason = ReasonRetriesExhausted
		}
		if reason != "" {
			c.logf(`{"event":"events_dead_lettered","level":"warn","queue":%q,"type":%q,"id":%q,"reason":%q,"attempts":%d,"error":%q}`,
				s.Queue, event.Type, event.ID, reason, attempt+1, c.errorText(err))
			if err := d.Nack(false, false); err != nil {
				c.ackFailed(s, err)
				return
			}
			c.metrics.deadLettered(s.Queue, reason)
			return
		}
		c.metrics.retried(s.Queue)
		c.logf(`{"event":"events_retry","level":"warn","queue":%q,"type":%q,"id":%q,"attempt":%d,"error":%q}`,
			s.Queue, event.Type, event.ID, attempt+1, c.errorText(err))
		if !sleep(ctx, c.retryDelays[attempt]) {
			return
		}
	}
}

// attempt runs the handler once in the record's transaction; a panic rolls
// the transaction back and comes back as an error.
func (c *Consumer) attempt(ctx context.Context, s *subscription, event Event) (duplicate bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			duplicate, err = false, panicError{r}
		}
	}()
	return c.store.Consume(ctx, s.Consumer, event.Source, event.ID, func(tx pgx.Tx) error {
		return s.Handle(ctx, tx, event)
	})
}

func (c *Consumer) ackFailed(s *subscription, err error) {
	c.logf(`{"event":"events_settle_failed","level":"warn","queue":%q,"error":%q}`, s.Queue, c.redact(err))
}

var (
	emailLike = regexp.MustCompile(`[^\s@"'<>(),;:]+@[^\s@"'<>(),;:]+\.[A-Za-z]{2,}`)
	ipv4Like  = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
)

// errorText is a handler's error as its log line carries it. Handlers wrap
// their errors with ids, but what they wrap can quote personal data: a
// PostgreSQL error's message (invalid input syntax for type uuid: "…") gives
// way to its SQLSTATE and constraint, a panic's value to its type (a runtime
// error keeps its text), e-mail and IPv4 addresses to a placeholder, and the
// password to [redacted].
func (c *Consumer) errorText(err error) string {
	text := c.redact(err)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		safe := "PostgreSQL error (SQLSTATE " + pgErr.Code + ")"
		if pgErr.ConstraintName != "" {
			safe = "PostgreSQL error (SQLSTATE " + pgErr.Code + ", constraint " + pgErr.ConstraintName + ")"
		}
		if inner := pgErr.Error(); strings.Contains(text, inner) {
			text = strings.ReplaceAll(text, inner, safe)
		} else {
			text = safe
		}
	}
	var p panicError
	if errors.As(err, &p) {
		text = p.logText()
	}
	text = emailLike.ReplaceAllString(text, "[e-mail]")
	return ipv4Like.ReplaceAllString(text, "[ip]")
}

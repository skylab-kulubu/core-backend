package events

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
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
		c.logf(`{"event":"events_dead_lettered","level":"warn","queue":%q,"reason":%q,"error":%q}`, s.Queue, ReasonInvalidEnvelope, err.Error())
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
				s.Queue, event.Type, event.ID, reason, attempt+1, err.Error())
			if err := d.Nack(false, false); err != nil {
				c.ackFailed(s, err)
				return
			}
			c.metrics.deadLettered(s.Queue, reason)
			return
		}
		c.metrics.retried(s.Queue)
		c.logf(`{"event":"events_retry","level":"warn","queue":%q,"type":%q,"id":%q,"attempt":%d,"error":%q}`,
			s.Queue, event.Type, event.ID, attempt+1, err.Error())
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

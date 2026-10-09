// Package outbox runs the relay that turns outbox messages into calls to the exchange
// and the gateway, and turns their answers back into order events.
//
// A message is claimed with a lease, sent after the claim commits, and marked sent in the
// same transaction as the event it produced. A relay that dies mid-way loses nothing:
// the lease runs out and another relay sends the message again under the same reference.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"
	"uuid"

	"github.com/DarkStark9000/allot/internal/exchange"
	"github.com/DarkStark9000/allot/internal/gateway"
	"github.com/DarkStark9000/allot/internal/order"
	"github.com/DarkStark9000/allot/internal/store"
)

// Exchange is what the relay needs from the exchange order platform.
type Exchange interface {
	Submit(ctx context.Context, req exchange.SubmitRequest) (exchange.Answer, error)
	Status(ctx context.Context, ref string) (exchange.Answer, error)
}

// Gateway is what the relay needs from the payment gateway.
type Gateway interface {
	Refund(ctx context.Context, req gateway.RefundRequest) error
}

// Config holds the relay's dependencies and timings.
type Config struct {
	DB       *store.DB
	Exchange Exchange
	Gateway  Gateway
	Logger   *slog.Logger
	// Name appears in logs and in the history of every event this relay applies.
	Name string
	// Batch is the most messages claimed at once.
	Batch int
	// Lease is how long a claimed message stays invisible to other relays.
	Lease time.Duration
	// Poll is the wait between empty claims.
	Poll time.Duration
	// CallTimeout bounds each call to the exchange or the gateway.
	CallTimeout time.Duration
	// BackoffBase and BackoffMax bound the wait before a failed message is tried again.
	BackoffBase, BackoffMax time.Duration
}

// Relay sends outbox messages.
type Relay struct {
	cfg Config
	log *slog.Logger
}

// New returns a relay. Zero timings get working defaults.
func New(cfg Config) *Relay {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Name == "" {
		cfg.Name = "relay"
	}
	defaults := []struct {
		field *time.Duration
		value time.Duration
	}{
		{&cfg.Lease, 30 * time.Second},
		{&cfg.Poll, 200 * time.Millisecond},
		{&cfg.CallTimeout, 5 * time.Second},
		{&cfg.BackoffBase, 100 * time.Millisecond},
		{&cfg.BackoffMax, 30 * time.Second},
	}
	for _, d := range defaults {
		if *d.field == 0 {
			*d.field = d.value
		}
	}
	if cfg.Batch == 0 {
		cfg.Batch = 16
	}
	return &Relay{cfg: cfg, log: cfg.Logger.With("relay", cfg.Name)}
}

// Run drains the outbox until ctx ends.
func (r *Relay) Run(ctx context.Context) error {
	for {
		n, err := r.Drain(ctx)
		if err != nil && ctx.Err() == nil {
			r.log.Error("drain", "error", err)
		}
		if n > 0 && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(r.cfg.Poll):
		}
	}
}

// Drain claims one batch and processes it. It returns how many messages it claimed.
func (r *Relay) Drain(ctx context.Context) (int, error) {
	msgs, err := r.cfg.DB.ClaimOutbox(ctx, r.cfg.Batch, r.cfg.Lease)
	if err != nil {
		return 0, err
	}
	var errs []error
	for _, m := range msgs {
		if err := r.process(ctx, m); err != nil {
			errs = append(errs, fmt.Errorf("message %d: %w", m.ID, err))
		}
	}
	return len(msgs), errors.Join(errs...)
}

func (r *Relay) process(ctx context.Context, m store.Message) error {
	o, err := r.cfg.DB.Order(ctx, m.OrderID)
	if err != nil {
		return err
	}
	switch m.Kind {
	case order.SubmitOrder:
		return r.submit(ctx, m, o)
	case order.QueryStatus:
		return r.queryStatus(ctx, m, o)
	case order.RefundPayment:
		return r.refund(ctx, m, o)
	}
	return r.cfg.DB.Reschedule(ctx, m.ID, r.cfg.BackoffMax, "unknown message kind "+string(m.Kind))
}

func submitRequest(o order.Order) exchange.SubmitRequest {
	return exchange.SubmitRequest{
		Ref:             o.ExchangeRef(),
		SchemeCode:      o.SchemeCode,
		Category:        string(o.Category),
		AmountPaise:     int64(o.Amount),
		OrderedAt:       o.CreatedAt,
		FundsRealisedAt: o.PaidAt,
	}
}

func (r *Relay) submit(ctx context.Context, m store.Message, o order.Order) error {
	if o.State != order.Submitted {
		return r.cfg.DB.MarkSent(ctx, m.ID) // the order moved on; nothing to send
	}
	call, cancel := context.WithTimeout(ctx, r.cfg.CallTimeout)
	defer cancel()
	ans, err := r.cfg.Exchange.Submit(call, submitRequest(o))
	switch {
	case errors.Is(err, exchange.ErrTimeout):
		// Not a failure: the exchange may have the order. Ask before doing anything else.
		return r.finish(ctx, m, o.ID, order.ExchangeTimedOut{})
	case err != nil:
		return r.retry(ctx, m, err)
	}
	return r.finish(ctx, m, o.ID, answerEvent(ans))
}

func (r *Relay) queryStatus(ctx context.Context, m store.Message, o order.Order) error {
	if o.State != order.SubmitUncertain {
		return r.cfg.DB.MarkSent(ctx, m.ID)
	}
	call, cancel := context.WithTimeout(ctx, r.cfg.CallTimeout)
	defer cancel()
	ans, err := r.cfg.Exchange.Status(call, o.ExchangeRef())
	if errors.Is(err, exchange.ErrNotFound) {
		// The exchange never received it. Send it again under the same reference.
		ans, err = r.cfg.Exchange.Submit(call, submitRequest(o))
	}
	if err != nil {
		return r.retry(ctx, m, err)
	}
	return r.finish(ctx, m, o.ID, answerEvent(ans))
}

func (r *Relay) refund(ctx context.Context, m store.Message, o order.Order) error {
	if o.State != order.RefundPending {
		return r.cfg.DB.MarkSent(ctx, m.ID)
	}
	call, cancel := context.WithTimeout(ctx, r.cfg.CallTimeout)
	defer cancel()
	err := r.cfg.Gateway.Refund(call, gateway.RefundRequest{
		Ref:         RefundRef(o.ID),
		OrderID:     o.ID.String(),
		PaymentID:   o.PaymentID,
		AmountPaise: int64(o.Amount),
	})
	if err != nil {
		return r.retry(ctx, m, err)
	}
	return r.cfg.DB.MarkSent(ctx, m.ID) // the refund's result arrives as a webhook
}

// RefundRef is the gateway reference for an order's refund. One order, one refund.
func RefundRef(id uuid.UUID) string { return "RF" + id.String() }

func answerEvent(a exchange.Answer) order.Event {
	if a.Status == exchange.Rejected {
		return order.ExchangeRejected{Reason: a.Reason}
	}
	return order.ExchangeAccepted{}
}

// finish marks the message sent and applies the event it produced, in one transaction.
func (r *Relay) finish(ctx context.Context, m store.Message, id uuid.UUID, e order.Event) error {
	return r.cfg.DB.WithTx(ctx, func(tx *store.Tx) error {
		if err := tx.MarkSent(ctx, m.ID); err != nil {
			return err
		}
		src := store.Source{Name: r.cfg.Name, EventID: fmt.Sprintf("outbox:%d:%d", m.ID, m.Attempts)}
		_, err := tx.ApplyOrFlag(ctx, id, e, src)
		return err
	})
}

func (r *Relay) retry(ctx context.Context, m store.Message, cause error) error {
	wait := Backoff(m.Attempts, r.cfg.BackoffBase, r.cfg.BackoffMax, rand.Float64)
	r.log.Warn("retry", "message", m.ID, "kind", m.Kind, "attempt", m.Attempts, "wait", wait, "error", cause)
	return r.cfg.DB.Reschedule(ctx, m.ID, wait, cause.Error())
}

// Backoff returns the wait before attempt n+1 after n failures, with full jitter: a random
// duration between zero and min(limit, base * 2^(n-1)). rnd returns a number in [0, 1).
func Backoff(n int, base, limit time.Duration, rnd func() float64) time.Duration {
	ceiling := limit
	if n < 1 {
		n = 1
	}
	if shift := n - 1; shift < 62 && base<<shift > 0 && base<<shift < limit {
		ceiling = base << shift
	}
	return time.Duration(rnd() * float64(ceiling))
}

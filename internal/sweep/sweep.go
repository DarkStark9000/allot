// Package sweep expires orders whose payment never arrived.
package sweep

import (
	"context"
	"log/slog"
	"time"

	"github.com/DarkStark9000/allot/internal/order"
	"github.com/DarkStark9000/allot/internal/store"
)

// Expirer moves orders that waited longer than Window for payment to expired.
// Money that arrives after that is refunded by the state machine.
type Expirer struct {
	DB     *store.DB
	Window time.Duration
	Every  time.Duration
	Logger *slog.Logger
	Now    func() time.Time
}

// Run expires orders every e.Every until ctx ends.
func (e Expirer) Run(ctx context.Context) error {
	t := time.NewTicker(e.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if _, err := e.Once(ctx); err != nil && ctx.Err() == nil {
				e.Logger.Error("expire", "error", err)
			}
		}
	}
}

// Once expires every overdue order and returns how many it expired.
func (e Expirer) Once(ctx context.Context) (int, error) {
	now := time.Now
	if e.Now != nil {
		now = e.Now
	}
	ids, err := e.DB.PendingOlderThan(ctx, now().Add(-e.Window), 500)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		err := e.DB.WithTx(ctx, func(tx *store.Tx) error {
			outcome, err := tx.ApplyOrFlag(ctx, id, order.PaymentExpired{}, store.Source{Name: "sweeper", EventID: "expire"})
			if outcome == "applied" {
				n++
			}
			return err
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

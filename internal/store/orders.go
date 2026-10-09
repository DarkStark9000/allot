package store

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/DarkStark9000/allot/internal/money"
	"github.com/DarkStark9000/allot/internal/navdate"
	"github.com/DarkStark9000/allot/internal/order"
)

const orderColumns = `id::text, user_id, scheme_code, category, amount_paise, state, version,
	payment_id, paid_at, nav_date::text, nav, units_milli, created_at`

func scanOrder(row pgx.Row) (order.Order, error) {
	var (
		o                  order.Order
		id                 string
		category, state    string
		paymentID, navDate *string
		paidAt             *time.Time
		nav, units         *int64
		amount             int64
	)
	err := row.Scan(&id, &o.UserID, &o.SchemeCode, &category, &amount, &state, &o.Version,
		&paymentID, &paidAt, &navDate, &nav, &units, &o.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return order.Order{}, ErrNotFound
	}
	if err != nil {
		return order.Order{}, err
	}
	if o.ID, err = uuid.Parse(id); err != nil {
		return order.Order{}, err
	}
	o.Category, o.State, o.Amount = navdate.Category(category), order.State(state), money.Paise(amount)
	if paymentID != nil {
		o.PaymentID = *paymentID
	}
	if paidAt != nil {
		o.PaidAt = *paidAt
	}
	if navDate != nil {
		if o.NAVDate, err = navdate.ParseDate(*navDate); err != nil {
			return order.Order{}, err
		}
	}
	if nav != nil {
		o.NAV = money.NAV(*nav)
	}
	if units != nil {
		o.Units = money.Units(*units)
	}
	return o, nil
}

// ErrDuplicateKey reports an order that already exists for this user and idempotency key.
var ErrDuplicateKey = errors.New("store: order exists for this idempotency key")

// InsertOrder stores a new order under the user's idempotency key.
func (t *Tx) InsertOrder(ctx context.Context, o order.Order, key string) error {
	_, err := t.tx.Exec(ctx, `INSERT INTO orders
		(id, user_id, idempotency_key, scheme_code, category, amount_paise, state, version, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		o.ID.String(), o.UserID, key, o.SchemeCode, string(o.Category), int64(o.Amount), string(o.State), o.Version, o.CreatedAt)
	if isUniqueViolation(err) {
		return ErrDuplicateKey
	}
	return err
}

// OrderByKey returns the order a user created with an idempotency key.
func (t *Tx) OrderByKey(ctx context.Context, userID, key string) (order.Order, error) {
	return scanOrder(t.tx.QueryRow(ctx, `SELECT `+orderColumns+` FROM orders
		WHERE user_id = $1 AND idempotency_key = $2`, userID, key))
}

// Order returns one order.
func (db *DB) Order(ctx context.Context, id uuid.UUID) (order.Order, error) {
	return scanOrder(db.pool.QueryRow(ctx, `SELECT `+orderColumns+` FROM orders WHERE id = $1`, id.String()))
}

// Source names where an event came from, so the history shows why each transition happened.
type Source struct {
	Name    string
	EventID string
}

// Apply locks the order, applies e, and stores the result: the order, one history row,
// its ledger postings, and its outbox messages. A stale or conflicting event writes
// nothing and returns the *order.TransitionError.
func (t *Tx) Apply(ctx context.Context, id uuid.UUID, e order.Event, src Source) (order.Result, error) {
	o, err := scanOrder(t.tx.QueryRow(ctx, `SELECT `+orderColumns+` FROM orders WHERE id = $1 FOR UPDATE`, id.String()))
	if err != nil {
		return order.Result{}, err
	}
	r, err := order.Apply(o, e)
	if err != nil {
		return r, err
	}
	n := r.Order
	var navDate *string
	if n.NAVDate != (navdate.Date{}) {
		s := n.NAVDate.String()
		navDate = &s
	}
	var paidAt *time.Time
	if !n.PaidAt.IsZero() {
		paidAt = &n.PaidAt
	}
	_, err = t.tx.Exec(ctx, `UPDATE orders SET state = $2, version = $3, payment_id = NULLIF($4, ''),
		paid_at = $5, nav_date = $6::date, nav = NULLIF($7::bigint, 0), units_milli = NULLIF($8::bigint, 0), updated_at = now()
		WHERE id = $1`,
		id.String(), string(n.State), n.Version, n.PaymentID, paidAt, navDate, int64(n.NAV), int64(n.Units))
	if err != nil {
		return order.Result{}, fmt.Errorf("store: update order: %w", err)
	}
	_, err = t.tx.Exec(ctx, `INSERT INTO order_events (order_id, seq, from_state, to_state, event_kind, source, event_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id.String(), n.Version, string(o.State), string(n.State), e.Kind(), src.Name, src.EventID)
	if err != nil {
		return order.Result{}, fmt.Errorf("store: history: %w", err)
	}
	for _, p := range r.Postings {
		_, err = t.tx.Exec(ctx, `INSERT INTO ledger_entries (order_id, seq, debit_account, credit_account, amount_paise)
			VALUES ($1, $2, $3, $4, $5)`, id.String(), n.Version, string(p.Debit), string(p.Credit), int64(p.Amount))
		if err != nil {
			return order.Result{}, fmt.Errorf("store: ledger: %w", err)
		}
	}
	for _, m := range r.Messages {
		if _, err = t.tx.Exec(ctx, `INSERT INTO outbox (order_id, kind) VALUES ($1, $2)`, id.String(), string(m.Kind)); err != nil {
			return order.Result{}, fmt.Errorf("store: outbox: %w", err)
		}
	}
	return r, nil
}

// ApplyOrFlag applies e and names the outcome: applied, stale, conflict, or unknown_order.
// A stale event is ignored. A conflicting event opens a finding for a person.
// Only database failures return an error.
func (t *Tx) ApplyOrFlag(ctx context.Context, id uuid.UUID, e order.Event, src Source) (string, error) {
	_, err := t.Apply(ctx, id, e, src)
	switch {
	case err == nil:
		return "applied", nil
	case errors.Is(err, order.ErrStale):
		return "stale", nil
	case errors.Is(err, order.ErrConflict):
		return "conflict", t.RecordFinding(ctx, id, "conflicting_event", err.Error())
	case errors.Is(err, ErrNotFound):
		return "unknown_order", nil
	}
	return "", err
}

// Transition is one row of an order's history.
type Transition struct {
	Seq       int         `json:"seq"`
	From      order.State `json:"from"`
	To        order.State `json:"to"`
	EventKind string      `json:"event"`
	Source    string      `json:"source"`
	At        time.Time   `json:"at"`
}

// History returns an order's transitions, oldest first.
func (db *DB) History(ctx context.Context, id uuid.UUID) ([]Transition, error) {
	rows, err := db.pool.Query(ctx, `SELECT seq, from_state, to_state, event_kind, source, at
		FROM order_events WHERE order_id = $1 ORDER BY seq`, id.String())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Transition, error) {
		var tr Transition
		var from, to string
		err := row.Scan(&tr.Seq, &from, &to, &tr.EventKind, &tr.Source, &tr.At)
		tr.From, tr.To = order.State(from), order.State(to)
		return tr, err
	})
}

// PendingOlderThan returns orders still waiting for payment that were created before cutoff.
func (db *DB) PendingOlderThan(ctx context.Context, cutoff time.Time, limit int) ([]uuid.UUID, error) {
	rows, err := db.pool.Query(ctx, `SELECT id::text FROM orders
		WHERE state = 'payment_pending' AND created_at < $1 ORDER BY created_at LIMIT $2`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, 0, len(ids))
	for _, s := range ids {
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

// OrdersIn returns orders in any of the given states, oldest first.
func (db *DB) OrdersIn(ctx context.Context, states ...order.State) ([]order.Order, error) {
	names := make([]string, len(states))
	for i, s := range states {
		names[i] = string(s)
	}
	rows, err := db.pool.Query(ctx, `SELECT `+orderColumns+` FROM orders WHERE state = ANY($1) ORDER BY created_at`, names)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (order.Order, error) { return scanOrder(row) })
}

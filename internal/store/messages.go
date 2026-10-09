package store

import (
	"context"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/DarkStark9000/allot/internal/order"
)

// RecordInbound stores an inbound event once. It returns false when the event was
// already recorded, which means it was already applied and must change nothing.
func (t *Tx) RecordInbound(ctx context.Context, source, eventID string, orderID uuid.UUID, kind string, payload []byte) (bool, error) {
	var id *string
	if orderID != (uuid.UUID{}) {
		s := orderID.String()
		id = &s
	}
	tag, err := t.tx.Exec(ctx, `INSERT INTO inbox_events (source, event_id, order_id, kind, payload)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (source, event_id) DO NOTHING`, source, eventID, id, kind, payload)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SetInboundOutcome records what an inbound event did: applied, stale, conflict, or unknown_order.
func (t *Tx) SetInboundOutcome(ctx context.Context, source, eventID, outcome string) error {
	_, err := t.tx.Exec(ctx, `UPDATE inbox_events SET outcome = $3 WHERE source = $1 AND event_id = $2`, source, eventID, outcome)
	return err
}

// Message is one unit of outbox work.
type Message struct {
	ID       int64
	OrderID  uuid.UUID
	Kind     order.MessageKind
	Attempts int
}

// ClaimOutbox leases up to limit messages that are due. Each claimed message is invisible
// to other relays until its lease runs out. The send happens after this transaction commits.
func (db *DB) ClaimOutbox(ctx context.Context, limit int, lease time.Duration) ([]Message, error) {
	rows, err := db.pool.Query(ctx, `UPDATE outbox
		SET lease_until = now() + make_interval(secs => $2), attempts = attempts + 1
		WHERE id IN (
			SELECT id FROM outbox
			WHERE sent_at IS NULL AND available_at <= now() AND (lease_until IS NULL OR lease_until < now())
			ORDER BY available_at, id
			LIMIT $1
			FOR UPDATE SKIP LOCKED)
		RETURNING id, order_id::text, kind, attempts`, limit, lease.Seconds())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Message, error) {
		var m Message
		var id, kind string
		if err := row.Scan(&m.ID, &id, &kind, &m.Attempts); err != nil {
			return m, err
		}
		m.Kind = order.MessageKind(kind)
		var err error
		m.OrderID, err = uuid.Parse(id)
		return m, err
	})
}

// MarkSent records that a message needs no more work.
func (t *Tx) MarkSent(ctx context.Context, id int64) error {
	_, err := t.tx.Exec(ctx, `UPDATE outbox SET sent_at = now(), lease_until = NULL WHERE id = $1`, id)
	return err
}

// MarkSent records that a message needs no more work.
func (db *DB) MarkSent(ctx context.Context, id int64) error {
	return db.WithTx(ctx, func(t *Tx) error { return t.MarkSent(ctx, id) })
}

// Reschedule makes a message due again after a delay and releases its lease.
func (db *DB) Reschedule(ctx context.Context, id int64, after time.Duration, cause string) error {
	_, err := db.pool.Exec(ctx, `UPDATE outbox
		SET available_at = now() + make_interval(secs => $2), lease_until = NULL, last_error = $3
		WHERE id = $1`, id, after.Seconds(), cause)
	return err
}

// UnsentMessages counts messages that still need work.
func (db *DB) UnsentMessages(ctx context.Context) (int, error) {
	var n int
	err := db.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE sent_at IS NULL`).Scan(&n)
	return n, err
}

// Finding is something a person must look at.
type Finding struct {
	OrderID uuid.UUID `json:"order_id"`
	Kind    string    `json:"kind"`
	Detail  string    `json:"detail"`
	FoundAt time.Time `json:"found_at"`
}

// RecordFinding opens a finding. It does nothing if the same finding is already open.
func (t *Tx) RecordFinding(ctx context.Context, orderID uuid.UUID, kind, detail string) error {
	_, err := t.tx.Exec(ctx, `INSERT INTO recon_findings (order_id, kind, detail) VALUES ($1, $2, $3)
		ON CONFLICT (order_id, kind) WHERE resolved_at IS NULL DO NOTHING`, orderID.String(), kind, detail)
	return err
}

// Findings returns every open finding, oldest first.
func (db *DB) Findings(ctx context.Context) ([]Finding, error) {
	rows, err := db.pool.Query(ctx, `SELECT order_id::text, kind, detail, found_at
		FROM recon_findings WHERE resolved_at IS NULL ORDER BY found_at, id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Finding, error) {
		var f Finding
		var id string
		if err := row.Scan(&id, &f.Kind, &f.Detail, &f.FoundAt); err != nil {
			return f, err
		}
		var err error
		f.OrderID, err = uuid.Parse(id)
		return f, err
	})
}

// Query runs a read-only query for checks and reports. It is not for application logic.
func (db *DB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return db.pool.Query(ctx, sql, args...)
}

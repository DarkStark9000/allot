package store

import (
	"bytes"
	"context"
	"time"
)

// ClaimOutcome is what happened when a request tried to claim an idempotency key.
type ClaimOutcome int

const (
	// Claimed means this request owns the key and must do the work.
	Claimed ClaimOutcome = iota
	// Replay means the work is done; answer with the stored response.
	Replay
	// InFlight means another request holds the key right now.
	InFlight
	// Mismatch means the key was used before with a different request.
	Mismatch
)

// Claim is the result of ClaimKey.
type Claim struct {
	Outcome ClaimOutcome
	Code    int
	Body    []byte
}

// ClaimKey claims a user's idempotency key for a request with the given fingerprint.
// The claim holds for lease. If the holder crashes, the lease runs out and the next
// request with the same key and fingerprint takes the key over.
func (db *DB) ClaimKey(ctx context.Context, userID, key string, fingerprint []byte, lease time.Duration) (Claim, error) {
	var c Claim
	err := db.WithTx(ctx, func(t *Tx) error {
		tag, err := t.tx.Exec(ctx, `INSERT INTO idempotency_keys (user_id, key, fingerprint, locked_until)
			VALUES ($1, $2, $3, now() + make_interval(secs => $4)) ON CONFLICT DO NOTHING`,
			userID, key, fingerprint, lease.Seconds())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			c = Claim{Outcome: Claimed}
			return nil
		}
		var (
			stored    []byte
			completed bool
			code      *int
			body      []byte
			expired   bool
		)
		err = t.tx.QueryRow(ctx, `SELECT fingerprint, completed, response_code, response_body,
			coalesce(locked_until < now(), true)
			FROM idempotency_keys WHERE user_id = $1 AND key = $2 FOR UPDATE`, userID, key).
			Scan(&stored, &completed, &code, &body, &expired)
		if err != nil {
			return err
		}
		switch {
		case !bytes.Equal(stored, fingerprint):
			c = Claim{Outcome: Mismatch}
		case completed && code != nil:
			c = Claim{Outcome: Replay, Code: *code, Body: body}
		case !expired:
			c = Claim{Outcome: InFlight}
		default:
			c = Claim{Outcome: Claimed}
			_, err = t.tx.Exec(ctx, `UPDATE idempotency_keys SET locked_until = now() + make_interval(secs => $3)
				WHERE user_id = $1 AND key = $2`, userID, key, lease.Seconds())
		}
		return err
	})
	return c, err
}

// CompleteKey stores the response for a claimed key, in the same transaction as the work.
func (t *Tx) CompleteKey(ctx context.Context, userID, key string, code int, body []byte) error {
	_, err := t.tx.Exec(ctx, `UPDATE idempotency_keys
		SET completed = true, response_code = $3, response_body = $4, locked_until = NULL
		WHERE user_id = $1 AND key = $2`, userID, key, code, body)
	return err
}

// CompleteKey stores a response that needed no other work, such as a validation error.
func (db *DB) CompleteKey(ctx context.Context, userID, key string, code int, body []byte) error {
	return db.WithTx(ctx, func(t *Tx) error { return t.CompleteKey(ctx, userID, key, code, body) })
}

// ReleaseKey gives up a claim after a failure that a retry may fix, such as a lost database.
func (db *DB) ReleaseKey(ctx context.Context, userID, key string) error {
	_, err := db.pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE user_id = $1 AND key = $2 AND NOT completed`, userID, key)
	return err
}

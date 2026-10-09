package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/DarkStark9000/allot/internal/navdate"
	"github.com/DarkStark9000/allot/internal/order"
	"github.com/DarkStark9000/allot/internal/store"
	"github.com/DarkStark9000/allot/internal/testdb"
)

func placeOrder(t *testing.T, db *store.DB, key string) order.Order {
	t.Helper()
	o, err := order.New(uuid.NewV7(), "user_1", "INF000K01ABC", navdate.Other, 5_000_00, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	err = db.WithTx(t.Context(), func(tx *store.Tx) error { return tx.InsertOrder(t.Context(), o, key) })
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func apply(t *testing.T, db *store.DB, id uuid.UUID, e order.Event, eventID string) (order.Result, error) {
	t.Helper()
	var r order.Result
	err := db.WithTx(t.Context(), func(tx *store.Tx) error {
		var err error
		r, err = tx.Apply(t.Context(), id, e, store.Source{Name: "test", EventID: eventID})
		return err
	})
	return r, err
}

func TestApplyWritesOrderHistoryLedgerAndOutbox(t *testing.T) {
	db := testdb.New(t)
	o := placeOrder(t, db, "k1")
	if _, err := apply(t, db, o.ID, order.PaymentSucceeded{PaymentID: "pay_1", At: time.Now()}, "e1"); err != nil {
		t.Fatal(err)
	}
	got, err := db.Order(t.Context(), o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != order.Submitted || got.Version != 1 || got.PaymentID != "pay_1" {
		t.Errorf("order = %+v", got)
	}
	h, err := db.History(t.Context(), o.ID)
	if err != nil || len(h) != 1 || h[0].From != order.PaymentPending || h[0].To != order.Submitted {
		t.Errorf("history = %+v, %v", h, err)
	}
	msgs, err := db.ClaimOutbox(t.Context(), 10, time.Minute)
	if err != nil || len(msgs) != 1 || msgs[0].Kind != order.SubmitOrder || msgs[0].OrderID != o.ID {
		t.Errorf("outbox = %+v, %v", msgs, err)
	}
}

func TestApplyStaleWritesNothing(t *testing.T) {
	db := testdb.New(t)
	o := placeOrder(t, db, "k1")
	if _, err := apply(t, db, o.ID, order.PaymentSucceeded{PaymentID: "pay_1", At: time.Now()}, "e1"); err != nil {
		t.Fatal(err)
	}
	_, err := apply(t, db, o.ID, order.PaymentDeclined{PaymentID: "pay_1"}, "e2")
	if !errors.Is(err, order.ErrStale) {
		t.Fatalf("got %v, want ErrStale", err)
	}
	if h, _ := db.History(t.Context(), o.ID); len(h) != 1 {
		t.Errorf("history has %d rows, want 1", len(h))
	}
}

func TestDuplicateKeyIsRejected(t *testing.T) {
	db := testdb.New(t)
	placeOrder(t, db, "k1")
	o, _ := order.New(uuid.NewV7(), "user_1", "S", navdate.Other, 100, time.Now())
	err := db.WithTx(t.Context(), func(tx *store.Tx) error { return tx.InsertOrder(t.Context(), o, "k1") })
	if !errors.Is(err, store.ErrDuplicateKey) {
		t.Fatalf("got %v, want ErrDuplicateKey", err)
	}
}

func TestClaimKey(t *testing.T) {
	db := testdb.New(t)
	ctx := t.Context()
	fp, other := []byte("fingerprint-a"), []byte("fingerprint-b")

	c, err := db.ClaimKey(ctx, "u", "k", fp, time.Minute)
	if err != nil || c.Outcome != store.Claimed {
		t.Fatalf("first claim = %+v, %v", c, err)
	}
	if c, _ = db.ClaimKey(ctx, "u", "k", fp, time.Minute); c.Outcome != store.InFlight {
		t.Errorf("second claim while held = %v, want InFlight", c.Outcome)
	}
	if c, _ = db.ClaimKey(ctx, "u", "k", other, time.Minute); c.Outcome != store.Mismatch {
		t.Errorf("different fingerprint = %v, want Mismatch", c.Outcome)
	}
	if c, _ = db.ClaimKey(ctx, "other_user", "k", other, time.Minute); c.Outcome != store.Claimed {
		t.Errorf("same key, other user = %v, want Claimed", c.Outcome)
	}
	if err := db.CompleteKey(ctx, "u", "k", 201, []byte(`{"id":"x"}`)); err != nil {
		t.Fatal(err)
	}
	c, _ = db.ClaimKey(ctx, "u", "k", fp, time.Minute)
	if c.Outcome != store.Replay || c.Code != 201 || string(c.Body) != `{"id":"x"}` {
		t.Errorf("after completion = %+v, want Replay 201", c)
	}
}

func TestClaimKeyTakesOverExpiredLease(t *testing.T) {
	db := testdb.New(t)
	ctx := t.Context()
	// A negative lease is a holder that crashed long ago.
	if c, _ := db.ClaimKey(ctx, "u", "k", []byte("fp"), -time.Minute); c.Outcome != store.Claimed {
		t.Fatalf("first claim = %v", c.Outcome)
	}
	if c, _ := db.ClaimKey(ctx, "u", "k", []byte("fp"), time.Minute); c.Outcome != store.Claimed {
		t.Errorf("claim after lease ran out = %v, want Claimed", c.Outcome)
	}
}

// TestOutbox_CrashBetweenCommitAndSend is failure mode F6: a relay claims a message and
// dies before sending it. The lease runs out and another relay gets the message.
func TestOutbox_CrashBetweenCommitAndSend(t *testing.T) {
	db := testdb.New(t)
	o := placeOrder(t, db, "k1")
	if _, err := apply(t, db, o.ID, order.PaymentSucceeded{PaymentID: "p", At: time.Now()}, "e1"); err != nil {
		t.Fatal(err)
	}
	crashed, err := db.ClaimOutbox(t.Context(), 10, -time.Second) // lease already over: the relay is gone
	if err != nil || len(crashed) != 1 {
		t.Fatalf("crashed relay claimed %d, %v", len(crashed), err)
	}
	next, err := db.ClaimOutbox(t.Context(), 10, time.Minute)
	if err != nil || len(next) != 1 || next[0].ID != crashed[0].ID || next[0].Attempts != 2 {
		t.Fatalf("next relay claimed %+v, %v; want the same message on attempt 2", next, err)
	}
	if again, _ := db.ClaimOutbox(t.Context(), 10, time.Minute); len(again) != 0 {
		t.Errorf("a leased message was claimed twice: %+v", again)
	}
}

// TestOutbox_ConcurrentRelays is failure mode F14: relays racing for the same messages
// each get a disjoint set.
func TestOutbox_ConcurrentRelays(t *testing.T) {
	db := testdb.New(t)
	const orders = 40
	for i := range orders {
		o := placeOrder(t, db, "k"+string(rune('a'+i%26))+string(rune('a'+i/26)))
		if _, err := apply(t, db, o.ID, order.PaymentSucceeded{PaymentID: "p", At: time.Now()}, "e"); err != nil {
			t.Fatal(err)
		}
	}
	var (
		mu   sync.Mutex
		seen = map[int64]int{}
		wg   sync.WaitGroup
	)
	for range 8 {
		wg.Go(func() {
			for {
				msgs, err := db.ClaimOutbox(context.Background(), 3, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if len(msgs) == 0 {
					return
				}
				mu.Lock()
				for _, m := range msgs {
					seen[m.ID]++
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(seen) != orders {
		t.Errorf("claimed %d distinct messages, want %d", len(seen), orders)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("message %d claimed %d times", id, n)
		}
	}
}

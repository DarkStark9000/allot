package sweep_test

import (
	"testing"
	"time"
	"uuid"

	"github.com/DarkStark9000/allot/internal/navdate"
	"github.com/DarkStark9000/allot/internal/order"
	"github.com/DarkStark9000/allot/internal/store"
	"github.com/DarkStark9000/allot/internal/sweep"
	"github.com/DarkStark9000/allot/internal/testdb"
)

// An order that waits past the payment window expires. Money that arrives after that
// moves it to refund_pending with a refund message, never to submitted.
func TestExpiryThenLatePaymentIsRefunded(t *testing.T) {
	db := testdb.New(t)
	ctx := t.Context()
	created := time.Now().Add(-time.Hour)
	o, err := order.New(uuid.NewV7(), "u", "S", navdate.Other, 1_000_00, created)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.WithTx(ctx, func(tx *store.Tx) error { return tx.InsertOrder(ctx, o, "k") }); err != nil {
		t.Fatal(err)
	}
	fresh, _ := order.New(uuid.NewV7(), "u", "S", navdate.Other, 1_000_00, time.Now())
	if err := db.WithTx(ctx, func(tx *store.Tx) error { return tx.InsertOrder(ctx, fresh, "k2") }); err != nil {
		t.Fatal(err)
	}

	n, err := sweep.Expirer{DB: db, Window: 15 * time.Minute}.Once(ctx)
	if err != nil || n != 1 {
		t.Fatalf("expired %d, %v; want 1", n, err)
	}
	if got, _ := db.Order(ctx, fresh.ID); got.State != order.PaymentPending {
		t.Errorf("an order inside the window expired: %s", got.State)
	}

	err = db.WithTx(ctx, func(tx *store.Tx) error {
		outcome, err := tx.ApplyOrFlag(ctx, o.ID, order.PaymentSucceeded{PaymentID: "late", At: time.Now()}, store.Source{Name: "gateway", EventID: "e"})
		if outcome != "applied" {
			t.Errorf("late payment outcome = %q", outcome)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := db.Order(ctx, o.ID)
	msgs, _ := db.ClaimOutbox(ctx, 10, time.Minute)
	if got.State != order.RefundPending || len(msgs) != 1 || msgs[0].Kind != order.RefundPayment {
		t.Errorf("after late money: %s with %+v, want refund_pending with one refund message", got.State, msgs)
	}
}

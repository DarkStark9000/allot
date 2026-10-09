package outbox_test

import (
	"math/rand/v2"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"

	"github.com/DarkStark9000/allot/internal/exchange"
	"github.com/DarkStark9000/allot/internal/fake"
	"github.com/DarkStark9000/allot/internal/gateway"
	"github.com/DarkStark9000/allot/internal/httpapi"
	"github.com/DarkStark9000/allot/internal/invariant"
	"github.com/DarkStark9000/allot/internal/ledger"
	"github.com/DarkStark9000/allot/internal/navdate"
	"github.com/DarkStark9000/allot/internal/order"
	"github.com/DarkStark9000/allot/internal/outbox"
	"github.com/DarkStark9000/allot/internal/store"
	"github.com/DarkStark9000/allot/internal/testdb"
)

var secret = []byte("relay-test")

type env struct {
	db    *store.DB
	ex    *fake.Exchange
	gw    *fake.Gateway
	relay *outbox.Relay
}

func setup(t *testing.T, f fake.ExchangeFaults) env {
	t.Helper()
	db := testdb.New(t)
	api := httptest.NewServer(httpapi.New(httpapi.Config{DB: db, WebhookSecret: secret, KeyLease: time.Minute}).Handler())
	t.Cleanup(api.Close)
	ex := fake.NewExchange(1, f)
	exSrv := httptest.NewServer(ex)
	t.Cleanup(func() { ex.Close(); exSrv.Close() })
	gw := fake.NewGateway(1, api.URL+"/v1/webhooks/payments", secret, fake.GatewayFaults{MaxCopies: 2})
	gwSrv := httptest.NewServer(gw)
	t.Cleanup(func() { gw.Close(); gwSrv.Close() })
	relay := outbox.New(outbox.Config{
		DB:          db,
		Exchange:    exchange.NewClient(exSrv.URL, nil),
		Gateway:     gateway.NewClient(gwSrv.URL, nil),
		CallTimeout: 300 * time.Millisecond,
		BackoffBase: time.Millisecond,
		BackoffMax:  time.Millisecond,
	})
	return env{db: db, ex: ex, gw: gw, relay: relay}
}

// paid creates an order and applies a successful payment, leaving one submit message.
func (e env) paid(t *testing.T) order.Order {
	t.Helper()
	o, err := order.New(uuid.NewV7(), "u", "INF000K01ABC", navdate.Other, 5_000_00, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	err = e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		if err := tx.InsertOrder(t.Context(), o, "k-"+o.ID.String()); err != nil {
			return err
		}
		_, err := tx.Apply(t.Context(), o.ID, order.PaymentSucceeded{PaymentID: "pay_1", At: time.Now()}, store.Source{Name: "test", EventID: "p"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// drain runs the relay until the outbox is empty, waiting out any backoff.
func (e env) drain(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := e.relay.Drain(t.Context()); err != nil {
			t.Fatal(err)
		}
		if n, _ := e.db.UnsentMessages(t.Context()); n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond) // backoff is 1 ms in these tests
	}
	t.Fatal("outbox did not drain")
}

func (e env) state(t *testing.T, id uuid.UUID) order.Order {
	t.Helper()
	o, err := e.db.Order(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestRelay_SubmitAccepted(t *testing.T) {
	e := setup(t, fake.ExchangeFaults{})
	o := e.paid(t)
	e.drain(t)
	if got := e.state(t, o.ID); got.State != order.Accepted {
		t.Fatalf("state = %s, want accepted", got.State)
	}
}

// F7: the exchange accepts the order and then never answers. The relay must not resubmit
// under a new reference; it asks for status and learns the order was accepted.
func TestExchange_TimeoutAfterAccept(t *testing.T) {
	e := setup(t, fake.ExchangeFaults{TimeoutAfterAccept: 1})
	o := e.paid(t)
	e.drain(t)
	got := e.state(t, o.ID)
	if got.State != order.Accepted {
		t.Fatalf("state = %s, want accepted", got.State)
	}
	h, _ := e.db.History(t.Context(), o.ID)
	if len(h) != 3 || h[1].To != order.SubmitUncertain {
		t.Errorf("history = %+v, want submitted, submit_uncertain, accepted", h)
	}
	if n := len(e.ex.Orders()); n != 1 {
		t.Errorf("exchange has %d orders, want 1", n)
	}
	if n := e.ex.Submissions()[o.ExchangeRef()]; n != 1 {
		t.Errorf("%d submissions, want 1: a status query is not a resubmission", n)
	}
}

// The exchange never sees the order. The status query finds nothing, so the relay sends
// the order again under the same reference, and the exchange records it once.
func TestExchange_DroppedThenResubmitted(t *testing.T) {
	e := setup(t, fake.ExchangeFaults{Drop: 1})
	o := e.paid(t)
	if _, err := e.relay.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := e.state(t, o.ID); got.State != order.SubmitUncertain {
		t.Fatalf("after the dropped call: %s, want submit_uncertain", got.State)
	}
	e.ex.SetFaults(fake.ExchangeFaults{})
	e.drain(t)
	if got := e.state(t, o.ID); got.State != order.Accepted {
		t.Fatalf("state = %s, want accepted", got.State)
	}
	if subs := e.ex.Submissions()[o.ExchangeRef()]; subs != 2 || len(e.ex.Orders()) != 1 {
		t.Errorf("submissions %d, exchange orders %d; want 2 and 1", subs, len(e.ex.Orders()))
	}
}

// F8: the exchange rejects a paid order. The money goes back and the ledger balances to zero.
func TestRefund_LedgerBalances(t *testing.T) {
	e := setup(t, fake.ExchangeFaults{Reject: 1})
	o := e.paid(t)
	e.drain(t)
	e.gw.Wait()
	got := e.state(t, o.ID)
	if got.State != order.Refunded {
		t.Fatalf("state = %s, want refunded", got.State)
	}
	ps, err := invariant.Ledger(t.Context(), e.db, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b := ledger.Balances(ps); len(b) != 0 {
		t.Errorf("balances after refund = %v, want all zero", b)
	}
	if n := len(e.gw.Refunds()); n != 1 {
		t.Errorf("gateway made %d refunds, want 1", n)
	}
}

func TestBackoff(t *testing.T) {
	base, limit := 100*time.Millisecond, 5*time.Second
	top := func() float64 { return 0.999999 }
	cases := map[int]time.Duration{1: base, 2: 2 * base, 3: 4 * base, 6: 32 * base, 7: limit, 200: limit}
	for n, ceiling := range cases {
		got := outbox.Backoff(n, base, limit, top)
		if got > ceiling || got < ceiling*99/100 {
			t.Errorf("Backoff(%d) at the top of the range = %v, want just under %v", n, got, ceiling)
		}
	}
	rnd := rand.New(rand.NewPCG(1, 2))
	for range 1000 {
		if d := outbox.Backoff(5, base, limit, rnd.Float64); d < 0 || d >= 16*base {
			t.Fatalf("Backoff(5) = %v outside [0, %v)", d, 16*base)
		}
	}
}

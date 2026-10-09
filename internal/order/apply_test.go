package order

import (
	"errors"
	"maps"
	"testing"
	"time"
	"uuid"

	"github.com/DarkStark9000/allot/internal/ledger"
	"github.com/DarkStark9000/allot/internal/money"
	"github.com/DarkStark9000/allot/internal/navdate"
)

var (
	paidAt   = time.Date(2026, time.October, 9, 11, 0, 0, 0, navdate.IST)
	allotted = AllotmentSeen{NAVDate: navdate.Date{Year: 2026, Month: time.October, Day: 9}, NAV: 61_7234, Units: 81_006}
)

// events are the inputs the table and the fuzz test draw from.
var events = map[string]Event{
	"paid":      PaymentSucceeded{PaymentID: "pay_1", At: paidAt},
	"paid2":     PaymentSucceeded{PaymentID: "pay_2", At: paidAt},
	"failed":    PaymentDeclined{PaymentID: "pay_1", Reason: "declined"},
	"expired":   PaymentExpired{},
	"accepted":  ExchangeAccepted{},
	"rejected":  ExchangeRejected{Reason: "scheme closed"},
	"timeout":   ExchangeTimedOut{},
	"allotted":  allotted,
	"allotted2": AllotmentSeen{NAVDate: allotted.NAVDate, NAV: allotted.NAV, Units: 99_000},
	"refunded":  RefundSucceeded{RefundID: "rf_1"},
}

const (
	stale    = "stale"
	conflict = "conflict"
)

// want[state][event] is the next state, or stale, or conflict.
var want = map[State]map[string]string{
	PaymentPending:  {"paid": "submitted", "paid2": "submitted", "failed": "payment_failed", "expired": "expired", "accepted": conflict, "rejected": conflict, "timeout": stale, "allotted": conflict, "allotted2": conflict, "refunded": conflict},
	Submitted:       {"paid": stale, "paid2": conflict, "failed": stale, "expired": stale, "accepted": "accepted", "rejected": "refund_pending", "timeout": "submit_uncertain", "allotted": "allotted", "allotted2": "allotted", "refunded": conflict},
	SubmitUncertain: {"paid": stale, "paid2": conflict, "failed": stale, "expired": stale, "accepted": "accepted", "rejected": "refund_pending", "timeout": stale, "allotted": "allotted", "allotted2": "allotted", "refunded": conflict},
	Accepted:        {"paid": stale, "paid2": conflict, "failed": stale, "expired": stale, "accepted": stale, "rejected": conflict, "timeout": stale, "allotted": "allotted", "allotted2": "allotted", "refunded": conflict},
	Allotted:        {"paid": stale, "paid2": conflict, "failed": stale, "expired": stale, "accepted": stale, "rejected": conflict, "timeout": stale, "allotted": stale, "allotted2": conflict, "refunded": conflict},
	PaymentFailed:   {"paid": "refund_pending", "paid2": "refund_pending", "failed": stale, "expired": stale, "accepted": conflict, "rejected": conflict, "timeout": stale, "allotted": conflict, "allotted2": conflict, "refunded": conflict},
	Expired:         {"paid": "refund_pending", "paid2": "refund_pending", "failed": stale, "expired": stale, "accepted": conflict, "rejected": conflict, "timeout": stale, "allotted": conflict, "allotted2": conflict, "refunded": conflict},
	RefundPending:   {"paid": stale, "paid2": conflict, "failed": stale, "expired": stale, "accepted": conflict, "rejected": stale, "timeout": stale, "allotted": conflict, "allotted2": conflict, "refunded": "refunded"},
	Refunded:        {"paid": stale, "paid2": conflict, "failed": stale, "expired": stale, "accepted": conflict, "rejected": stale, "timeout": stale, "allotted": conflict, "allotted2": conflict, "refunded": stale},
}

func newOrder(t testing.TB) Order {
	t.Helper()
	o, err := New(uuid.NewV7(), "user_1", "INF000K01ABC", navdate.Other, 5_000_00, paidAt)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// inState returns an order in s, with the fields an order in s would have.
func inState(t testing.TB, s State) Order {
	o := newOrder(t)
	o.State = s
	if s != PaymentPending && s != PaymentFailed && s != Expired {
		o.PaymentID, o.PaidAt = "pay_1", paidAt
	}
	if s == Allotted {
		o.NAVDate, o.NAV, o.Units = allotted.NAVDate, allotted.NAV, allotted.Units
	}
	return o
}

func TestApplyTable(t *testing.T) {
	if len(want) != len(States) {
		t.Fatalf("table covers %d states, the machine has %d", len(want), len(States))
	}
	for _, s := range States {
		for name, e := range events {
			t.Run(string(s)+"/"+name, func(t *testing.T) {
				o := inState(t, s)
				r, err := Apply(o, e)
				expect := want[s][name]
				switch expect {
				case stale, conflict:
					sentinel := map[string]error{stale: ErrStale, conflict: ErrConflict}[expect]
					var te *TransitionError
					if !errors.Is(err, sentinel) || !errors.As(err, &te) || te.From != s {
						t.Fatalf("got %v, want %s", err, expect)
					}
					if r.Order != o || r.Postings != nil || r.Messages != nil {
						t.Error("a rejected event changed the result")
					}
				default:
					if err != nil {
						t.Fatalf("got %v, want %s", err, expect)
					}
					if string(r.Order.State) != expect {
						t.Fatalf("state = %s, want %s", r.Order.State, expect)
					}
					if r.Order.Version != o.Version+1 {
						t.Errorf("version = %d, want %d", r.Order.Version, o.Version+1)
					}
				}
			})
		}
	}
}

func TestEffects(t *testing.T) {
	o := newOrder(t)
	r, err := Apply(o, events["paid"])
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Messages) != 1 || r.Messages[0].Kind != SubmitOrder {
		t.Errorf("payment: messages = %v, want one submit_order", r.Messages)
	}
	if r.Order.PaymentID != "pay_1" || !r.Order.PaidAt.Equal(paidAt) {
		t.Error("payment: payment ID or time not recorded")
	}
	r, err = Apply(r.Order, events["timeout"])
	if err != nil || len(r.Messages) != 1 || r.Messages[0].Kind != QueryStatus {
		t.Errorf("timeout: %v, messages = %v, want one query_status", err, r.Messages)
	}
	r, err = Apply(r.Order, events["rejected"])
	if err != nil || len(r.Messages) != 1 || r.Messages[0].Kind != RefundPayment {
		t.Errorf("rejected: %v, messages = %v, want one refund_payment", err, r.Messages)
	}
	if got := r.Order.ExchangeRef(); got != "ALT"+o.ID.String() {
		t.Errorf("exchange reference = %q", got)
	}
}

func TestNewValidates(t *testing.T) {
	id := uuid.NewV7()
	bad := []struct {
		user, scheme string
		c            navdate.Category
		amount       money.Paise
	}{
		{"", "S", navdate.Other, 100},
		{"u", "", navdate.Other, 100},
		{"u", "S", "gold", 100},
		{"u", "S", navdate.Other, 0},
	}
	for _, b := range bad {
		if _, err := New(id, b.user, b.scheme, b.c, b.amount, paidAt); err == nil {
			t.Errorf("New(%q, %q, %q, %d): want error", b.user, b.scheme, b.c, b.amount)
		}
	}
}

// FuzzLifecycle drives one order through any sequence of events and checks, after every
// step, that the order only made legal moves and that its ledger matches its state.
func FuzzLifecycle(f *testing.F) {
	names := []string{"paid", "paid2", "failed", "expired", "accepted", "rejected", "timeout", "allotted", "allotted2", "refunded"}
	f.Add([]byte{0, 6, 7})          // paid, timeout, allotted
	f.Add([]byte{3, 0, 9})          // expired, late payment, refunded
	f.Add([]byte{0, 0, 5, 5, 9, 9}) // duplicates everywhere
	f.Add([]byte{0, 6, 4, 7, 7, 8}) // uncertain, accepted, allotted twice, different units
	f.Fuzz(func(t *testing.T, script []byte) {
		o := newOrder(t)
		var postings []ledger.Posting
		for _, b := range script {
			e := events[names[int(b)%len(names)]]
			r, err := Apply(o, e)
			if err != nil {
				if !errors.Is(err, ErrStale) && !errors.Is(err, ErrConflict) {
					t.Fatalf("%s in %s: unexpected error %v", e.Kind(), o.State, err)
				}
				continue
			}
			if !CanMove(o.State, r.Order.State) {
				t.Fatalf("%s moved %s to %s", e.Kind(), o.State, r.Order.State)
			}
			if o.State.Terminal() && o.State != PaymentFailed && o.State != Expired {
				t.Fatalf("%s moved terminal state %s", e.Kind(), o.State)
			}
			postings = append(postings, r.Postings...)
			o = r.Order
			if got, want := ledger.Balances(postings), o.ExpectedBalances(); !maps.Equal(got, want) {
				t.Fatalf("after %s in %s: ledger %v, want %v", e.Kind(), o.State, got, want)
			}
		}
	})
}

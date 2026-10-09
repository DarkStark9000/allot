package order

import (
	"errors"
	"fmt"
	"time"

	"github.com/DarkStark9000/allot/internal/ledger"
	"github.com/DarkStark9000/allot/internal/money"
	"github.com/DarkStark9000/allot/internal/navdate"
)

// Event is something that happened outside the order: a webhook, an exchange answer,
// a row in a registrar file, or a timer.
type Event interface {
	Kind() string
}

// PaymentSucceeded says the gateway collected the money.
type PaymentSucceeded struct {
	PaymentID string
	At        time.Time
}

// PaymentDeclined says the gateway could not collect the money.
type PaymentDeclined struct {
	PaymentID string
	Reason    string
}

// PaymentExpired says no payment arrived within the payment window.
type PaymentExpired struct{}

// ExchangeAccepted says the exchange accepted the order.
type ExchangeAccepted struct{}

// ExchangeRejected says the exchange rejected the order.
type ExchangeRejected struct {
	Reason string
}

// ExchangeTimedOut says the submission got no answer. The exchange may or may not have the order.
type ExchangeTimedOut struct{}

// AllotmentSeen says the registrar's file shows units allotted for the order.
type AllotmentSeen struct {
	NAVDate navdate.Date
	NAV     money.NAV
	Units   money.Units
}

// RefundSucceeded says the gateway returned the money to the investor.
type RefundSucceeded struct {
	RefundID string
}

// Kind implements Event.
func (PaymentSucceeded) Kind() string { return "payment.succeeded" }

// Kind implements Event.
func (PaymentDeclined) Kind() string { return "payment.failed" }

// Kind implements Event.
func (PaymentExpired) Kind() string { return "payment.expired" }

// Kind implements Event.
func (ExchangeAccepted) Kind() string { return "exchange.accepted" }

// Kind implements Event.
func (ExchangeRejected) Kind() string { return "exchange.rejected" }

// Kind implements Event.
func (ExchangeTimedOut) Kind() string { return "exchange.timed_out" }

// Kind implements Event.
func (AllotmentSeen) Kind() string { return "rta.allotted" }

// Kind implements Event.
func (RefundSucceeded) Kind() string { return "refund.succeeded" }

// MessageKind names work for the outbox relay.
type MessageKind string

// The relay's three jobs: send an order, ask about an order, and return a payment.
const (
	SubmitOrder   MessageKind = "submit_order"
	QueryStatus   MessageKind = "query_status"
	RefundPayment MessageKind = "refund_payment"
)

// Message is work for the outbox relay. Its payload is read from the order when it is sent.
type Message struct {
	Kind MessageKind
}

// Result is an order after one event, with the side effects of the transition.
type Result struct {
	Order    Order
	Postings []ledger.Posting
	Messages []Message
}

var (
	// ErrStale marks an event that the order has already moved past. It is safe to ignore.
	ErrStale = errors.New("stale event")
	// ErrConflict marks an event that contradicts what the order already knows. A person must look.
	ErrConflict = errors.New("conflicting event")
)

// TransitionError says why an event does not apply to an order.
type TransitionError struct {
	From  State
	Event string
	Err   error
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("order: %s in state %s: %v", e.Event, e.From, e.Err)
}

func (e *TransitionError) Unwrap() error { return e.Err }

// Apply returns the order after event e, or a *TransitionError that wraps ErrStale or ErrConflict.
func Apply(o Order, e Event) (Result, error) {
	r, err := apply(o, e)
	if err != nil {
		return Result{Order: o}, &TransitionError{From: o.State, Event: e.Kind(), Err: err}
	}
	if !CanMove(o.State, r.Order.State) {
		// Unreachable unless apply and the moves table disagree; the tests keep them in step.
		panic(fmt.Sprintf("order: %s moved %s to %s outside the state machine", e.Kind(), o.State, r.Order.State))
	}
	r.Order.Version = o.Version + 1
	return r, nil
}

func apply(o Order, e Event) (Result, error) {
	switch e := e.(type) {
	case PaymentSucceeded:
		return paymentSucceeded(o, e)
	case PaymentDeclined:
		return to(o, PaymentFailed, ErrStale, PaymentPending)
	case PaymentExpired:
		return to(o, Expired, ErrStale, PaymentPending)
	case ExchangeAccepted:
		return exchangeAccepted(o)
	case ExchangeRejected:
		return exchangeRejected(o)
	case ExchangeTimedOut:
		r, err := to(o, SubmitUncertain, ErrStale, Submitted)
		if err != nil {
			return r, err
		}
		r.Messages = []Message{{Kind: QueryStatus}}
		return r, nil
	case AllotmentSeen:
		return allotmentSeen(o, e)
	case RefundSucceeded:
		if o.State == Refunded {
			return Result{}, ErrStale
		}
		r, err := to(o, Refunded, ErrConflict, RefundPending)
		if err != nil {
			return r, err
		}
		r.Postings = []ledger.Posting{{Debit: ledger.Investor, Credit: ledger.Clearing, Amount: o.Amount}}
		return r, nil
	}
	return Result{}, fmt.Errorf("unknown event %T: %w", e, ErrConflict)
}

// to moves o to next if it is in one of from, and otherwise fails with otherwise.
func to(o Order, next State, otherwise error, from ...State) (Result, error) {
	for _, s := range from {
		if o.State == s {
			o.State = next
			return Result{Order: o}, nil
		}
	}
	return Result{}, otherwise
}

func paymentSucceeded(o Order, e PaymentSucceeded) (Result, error) {
	collect := []ledger.Posting{{Debit: ledger.Clearing, Credit: ledger.Investor, Amount: o.Amount}}
	switch o.State {
	case PaymentPending:
		o.State, o.PaymentID, o.PaidAt = Submitted, e.PaymentID, e.At
		return Result{Order: o, Postings: collect, Messages: []Message{{Kind: SubmitOrder}}}, nil
	case PaymentFailed, Expired:
		// The money arrived after the order gave up on it: return it.
		o.State, o.PaymentID, o.PaidAt = RefundPending, e.PaymentID, e.At
		return Result{Order: o, Postings: collect, Messages: []Message{{Kind: RefundPayment}}}, nil
	}
	if e.PaymentID != o.PaymentID {
		// A second, different payment for one order. It must be returned by hand.
		return Result{}, ErrConflict
	}
	return Result{}, ErrStale
}

func exchangeAccepted(o Order) (Result, error) {
	switch o.State {
	case Submitted, SubmitUncertain:
		o.State = Accepted
		return Result{Order: o}, nil
	case Accepted, Allotted:
		return Result{}, ErrStale
	}
	return Result{}, ErrConflict
}

func exchangeRejected(o Order) (Result, error) {
	switch o.State {
	case Submitted, SubmitUncertain:
		o.State = RefundPending
		return Result{Order: o, Messages: []Message{{Kind: RefundPayment}}}, nil
	case RefundPending, Refunded:
		return Result{}, ErrStale
	}
	return Result{}, ErrConflict
}

func allotmentSeen(o Order, e AllotmentSeen) (Result, error) {
	switch o.State {
	case Submitted, SubmitUncertain, Accepted:
		// Units in the registrar's file prove acceptance, even if that message never came.
		o.State, o.NAVDate, o.NAV, o.Units = Allotted, e.NAVDate, e.NAV, e.Units
		return Result{
			Order:    o,
			Postings: []ledger.Posting{{Debit: ledger.Fund, Credit: ledger.Clearing, Amount: o.Amount}},
		}, nil
	case Allotted:
		if o.NAVDate == e.NAVDate && o.NAV == e.NAV && o.Units == e.Units {
			return Result{}, ErrStale
		}
	}
	return Result{}, ErrConflict
}

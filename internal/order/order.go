// Package order is the state machine of a mutual fund purchase.
//
// It has no database, no clock, and no I/O. Apply takes an order and an event and returns
// the next order with its side effects as data: ledger postings and outbox messages.
// Callers persist all of it in one transaction.
package order

import (
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/DarkStark9000/allot/internal/ledger"
	"github.com/DarkStark9000/allot/internal/money"
	"github.com/DarkStark9000/allot/internal/navdate"
)

// State is where an order is in its life.
type State string

// The states of an order. README.md draws the moves between them.
const (
	PaymentPending  State = "payment_pending"
	Submitted       State = "submitted"
	SubmitUncertain State = "submit_uncertain"
	Accepted        State = "accepted"
	Allotted        State = "allotted"
	PaymentFailed   State = "payment_failed"
	Expired         State = "expired"
	RefundPending   State = "refund_pending"
	Refunded        State = "refunded"
)

// States lists every state, in the order the README draws them.
var States = []State{PaymentPending, Submitted, SubmitUncertain, Accepted, Allotted, PaymentFailed, Expired, RefundPending, Refunded}

// Terminal reports whether no event can move an order out of s.
// PaymentFailed and Expired are terminal for the investor, but money that arrives
// late still moves the order to RefundPending.
func (s State) Terminal() bool {
	switch s {
	case Allotted, Refunded, PaymentFailed, Expired:
		return true
	}
	return false
}

var moves = map[State][]State{
	PaymentPending:  {Submitted, PaymentFailed, Expired},
	Submitted:       {SubmitUncertain, Accepted, Allotted, RefundPending},
	SubmitUncertain: {Accepted, Allotted, RefundPending},
	Accepted:        {Allotted},
	PaymentFailed:   {RefundPending},
	Expired:         {RefundPending},
	RefundPending:   {Refunded},
}

// CanMove reports whether the state machine allows a move from one state to another.
func CanMove(from, to State) bool {
	for _, s := range moves[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Order is one purchase of one scheme.
type Order struct {
	ID         uuid.UUID
	UserID     string
	SchemeCode string
	Category   navdate.Category
	Amount     money.Paise
	State      State
	// Version counts the transitions applied so far.
	Version   int
	PaymentID string
	PaidAt    time.Time
	NAVDate   navdate.Date
	NAV       money.NAV
	Units     money.Units
	CreatedAt time.Time
}

// ExchangeRef is the order's reference at the exchange. It never changes, so every retry
// and every status query names the same order.
func (o Order) ExchangeRef() string { return "ALT" + o.ID.String() }

// New returns an order waiting for payment.
func New(id uuid.UUID, userID, schemeCode string, c navdate.Category, amount money.Paise, now time.Time) (Order, error) {
	switch {
	case userID == "":
		return Order{}, errors.New("order: user ID is required")
	case schemeCode == "":
		return Order{}, errors.New("order: scheme code is required")
	case c != navdate.Liquid && c != navdate.Overnight && c != navdate.Other:
		return Order{}, fmt.Errorf("order: unknown category %q", c)
	case amount <= 0:
		return Order{}, fmt.Errorf("order: amount must be positive, got %s", amount)
	}
	return Order{
		ID:         id,
		UserID:     userID,
		SchemeCode: schemeCode,
		Category:   c,
		Amount:     amount,
		State:      PaymentPending,
		CreatedAt:  now,
	}, nil
}

// ExpectedBalances returns the ledger balances an order in its current state must have.
// The chaos harness compares these with the postings stored in the database.
func (o Order) ExpectedBalances() map[ledger.Account]money.Paise {
	switch o.State {
	case Submitted, SubmitUncertain, Accepted, RefundPending:
		return map[ledger.Account]money.Paise{ledger.Clearing: o.Amount, ledger.Investor: -o.Amount}
	case Allotted:
		return map[ledger.Account]money.Paise{ledger.Fund: o.Amount, ledger.Investor: -o.Amount}
	default:
		return map[ledger.Account]money.Paise{}
	}
}

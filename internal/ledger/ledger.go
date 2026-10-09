// Package ledger records where an order's money is, in double entry.
package ledger

import "github.com/DarkStark9000/allot/internal/money"

// Account is a place money can be.
type Account string

const (
	// Investor is the investor's own bank account.
	Investor Account = "investor"
	// Clearing holds money between payment and allotment or refund.
	Clearing Account = "clearing"
	// Fund is money invested in the scheme.
	Fund Account = "fund"
)

// Posting moves Amount from the Credit account to the Debit account.
// Each posting balances by construction: one debit, one credit, one amount.
type Posting struct {
	Debit  Account
	Credit Account
	Amount money.Paise
}

// Balances returns the net balance of each account, debits minus credits.
// Accounts with a zero balance are left out, so two balance maps compare with maps.Equal.
func Balances(ps []Posting) map[Account]money.Paise {
	b := make(map[Account]money.Paise)
	for _, p := range ps {
		b[p.Debit] += p.Amount
		b[p.Credit] -= p.Amount
	}
	for a, v := range b {
		if v == 0 {
			delete(b, a)
		}
	}
	return b
}

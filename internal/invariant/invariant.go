// Package invariant checks the six guarantees across the database and the outside systems.
// The chaos harness runs it after every run; tests use its pieces.
package invariant

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"uuid"

	"github.com/DarkStark9000/allot/internal/exchange"
	"github.com/DarkStark9000/allot/internal/ledger"
	"github.com/DarkStark9000/allot/internal/money"
	"github.com/DarkStark9000/allot/internal/navdate"
	"github.com/DarkStark9000/allot/internal/order"
	"github.com/DarkStark9000/allot/internal/store"
)

// Violation is one broken guarantee.
type Violation struct {
	Guarantee string `json:"guarantee"`
	OrderID   string `json:"order_id,omitzero"`
	Detail    string `json:"detail"`
}

// External is what the outside systems recorded.
type External struct {
	// Exchange maps each exchange reference to the exchange's decision.
	Exchange map[string]exchange.Status
	// Refunds holds every refund reference the gateway paid out.
	Refunds map[string]bool
	// RefundRef names an order's refund reference.
	RefundRef func(uuid.UUID) string
}

// Summary describes the final state of a run.
type Summary struct {
	Orders   int            `json:"orders"`
	States   map[string]int `json:"states"`
	Findings map[string]int `json:"findings"`
}

type event struct {
	seq      int
	from, to order.State
}

// Check verifies G1 to G6 and returns every violation found.
func Check(ctx context.Context, db *store.DB, ext External, rules navdate.Rules, cal navdate.Calendar) ([]Violation, Summary, error) {
	orders, err := db.OrdersIn(ctx, order.States...)
	if err != nil {
		return nil, Summary{}, err
	}
	history, err := loadHistory(ctx, db)
	if err != nil {
		return nil, Summary{}, err
	}
	postings, err := loadLedger(ctx, db)
	if err != nil {
		return nil, Summary{}, err
	}
	findings, err := db.Findings(ctx)
	if err != nil {
		return nil, Summary{}, err
	}
	flagged := map[uuid.UUID]bool{}
	sum := Summary{Orders: len(orders), States: map[string]int{}, Findings: map[string]int{}}
	for _, f := range findings {
		flagged[f.OrderID] = true
		sum.Findings[f.Kind]++
	}

	var vs []Violation
	add := func(g string, id uuid.UUID, format string, args ...any) {
		vs = append(vs, Violation{Guarantee: g, OrderID: id.String(), Detail: fmt.Sprintf(format, args...)})
	}
	dupKeys, err := duplicateKeys(ctx, db)
	if err != nil {
		return nil, sum, err
	}
	for _, d := range dupKeys {
		vs = append(vs, Violation{Guarantee: "G1", Detail: d})
	}
	dupEvents, err := duplicateEffects(ctx, db)
	if err != nil {
		return nil, sum, err
	}
	for _, d := range dupEvents {
		vs = append(vs, Violation{Guarantee: "G2", Detail: d})
	}
	known := map[string]bool{}
	for _, o := range orders {
		sum.States[string(o.State)]++
		known[o.ExchangeRef()] = true
		checkHistory(o, history[o.ID], add)
		if got, want := ledger.Balances(postings[o.ID]), o.ExpectedBalances(); !maps.Equal(got, want) {
			add("G4", o.ID, "state %s: ledger %v, want %v", o.State, got, want)
		}
		checkExternal(o, ext, add)
		if !o.State.Terminal() && !flagged[o.ID] {
			add("G5", o.ID, "state %s is not terminal and has no finding", o.State)
		}
		if o.State == order.Allotted {
			d, err := rules.ForPurchase(o.Category, o.CreatedAt, o.PaidAt, cal)
			if err != nil || d.NAVDate != o.NAVDate {
				add("G6", o.ID, "NAV date %s, rules give %s", o.NAVDate, d.NAVDate)
			}
		}
	}
	for ref := range ext.Exchange {
		if !known[ref] {
			vs = append(vs, Violation{Guarantee: "G4", Detail: "exchange has an order allot never made: " + ref})
		}
	}
	return vs, sum, nil
}

func checkHistory(o order.Order, h []event, add func(string, uuid.UUID, string, ...any)) {
	prev := order.PaymentPending
	for i, e := range h {
		if e.seq != i+1 {
			add("G3", o.ID, "history seq %d at position %d", e.seq, i+1)
			return
		}
		if e.from != prev || !order.CanMove(e.from, e.to) {
			add("G3", o.ID, "illegal move %s to %s at seq %d", e.from, e.to, e.seq)
			return
		}
		prev = e.to
	}
	if prev != o.State || len(h) != o.Version {
		add("G3", o.ID, "history ends in %s after %d moves; order is %s v%d", prev, len(h), o.State, o.Version)
	}
}

func checkExternal(o order.Order, ext External, add func(string, uuid.UUID, string, ...any)) {
	status, atExchange := ext.Exchange[o.ExchangeRef()]
	refunded := ext.RefundRef != nil && ext.Refunds[ext.RefundRef(o.ID)]
	switch o.State {
	case order.Accepted, order.Allotted:
		if status != exchange.Accepted {
			add("G4", o.ID, "state %s but exchange has %q", o.State, status)
		}
	case order.Refunded:
		if !refunded {
			add("G4", o.ID, "refunded but the gateway paid no refund")
		}
		if atExchange && status == exchange.Accepted {
			add("G4", o.ID, "refunded but the exchange accepted the order")
		}
	case order.PaymentPending, order.PaymentFailed, order.Expired:
		if atExchange {
			add("G4", o.ID, "state %s but the exchange has the order", o.State)
		}
	}
	if refunded && o.State != order.Refunded && o.State != order.RefundPending {
		add("G4", o.ID, "the gateway refunded an order in state %s", o.State)
	}
}

func loadHistory(ctx context.Context, db *store.DB) (map[uuid.UUID][]event, error) {
	rows, err := db.Query(ctx, `SELECT order_id::text, seq, from_state, to_state FROM order_events ORDER BY order_id, seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID][]event{}
	for rows.Next() {
		var id, from, to string
		var e event
		if err := rows.Scan(&id, &e.seq, &from, &to); err != nil {
			return nil, err
		}
		e.from, e.to = order.State(from), order.State(to)
		oid, err := uuid.Parse(id)
		if err != nil {
			return nil, err
		}
		out[oid] = append(out[oid], e)
	}
	return out, rows.Err()
}

func loadLedger(ctx context.Context, db *store.DB) (map[uuid.UUID][]ledger.Posting, error) {
	rows, err := db.Query(ctx, `SELECT order_id::text, debit_account, credit_account, amount_paise FROM ledger_entries`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID][]ledger.Posting{}
	for rows.Next() {
		var id, debit, credit string
		var amount int64
		if err := rows.Scan(&id, &debit, &credit, &amount); err != nil {
			return nil, err
		}
		oid, err := uuid.Parse(id)
		if err != nil {
			return nil, err
		}
		out[oid] = append(out[oid], ledger.Posting{Debit: ledger.Account(debit), Credit: ledger.Account(credit), Amount: money.Paise(amount)})
	}
	return out, rows.Err()
}

// Ledger returns the postings of one order.
func Ledger(ctx context.Context, db *store.DB, id uuid.UUID) ([]ledger.Posting, error) {
	all, err := loadLedger(ctx, db)
	return all[id], err
}

func queryStrings(ctx context.Context, db *store.DB, sql string) ([]string, error) {
	rows, err := db.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func duplicateKeys(ctx context.Context, db *store.DB) ([]string, error) {
	return queryStrings(ctx, db, `SELECT user_id || '/' || idempotency_key || ' has ' || count(*) || ' orders'
		FROM orders GROUP BY user_id, idempotency_key HAVING count(*) > 1`)
}

// duplicateEffects finds an inbound event that changed an order more than once.
func duplicateEffects(ctx context.Context, db *store.DB) ([]string, error) {
	out, err := queryStrings(ctx, db, `SELECT source || ' event ' || event_id || ' applied ' || count(*) || ' times'
		FROM order_events WHERE source IN ('gateway', 'rta')
		GROUP BY source, event_id HAVING count(*) > 1`)
	slices.Sort(out)
	return out, err
}

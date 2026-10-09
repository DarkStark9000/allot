// Package recon reconciles the registrar's allotment file with allot's own orders.
//
// Each row is checked before it is applied: the units must equal amount over NAV, and the
// NAV date must match the cut-off rules. A row that fails a check opens a finding instead.
// Paid orders with no row by the deadline open a finding too.
package recon

import (
	"context"
	"encoding/csv"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/DarkStark9000/allot/internal/money"
	"github.com/DarkStark9000/allot/internal/navdate"
	"github.com/DarkStark9000/allot/internal/order"
	"github.com/DarkStark9000/allot/internal/store"
)

// Finding kinds.
const (
	UnitMismatch     = "unit_mismatch"
	NAVDateMismatch  = "nav_date_mismatch"
	MissingAllotment = "missing_allotment"
	UnknownReference = "unknown_reference"
)

// Report counts what one run did.
type Report struct {
	Rows       int            `json:"rows"`
	Outcomes   map[string]int `json:"outcomes"`
	NewMissing int            `json:"missing"`
}

// Row is one line of an allotment file.
type Row struct {
	Ref         string       `json:"exchange_ref"`
	SchemeCode  string       `json:"scheme_code"`
	AmountPaise int64        `json:"amount_paise"`
	NAVDate     navdate.Date `json:"nav_date"`
	NAV         money.NAV    `json:"nav"`
	Units       money.Units  `json:"units"`
}

var header = []string{"exchange_ref", "scheme_code", "amount_paise", "nav_date", "nav", "units"}

// ParseFile reads an allotment file. It rejects the whole file if any row is malformed.
func ParseFile(r io.Reader) ([]Row, error) {
	recs, err := csv.NewReader(r).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("recon: %w", err)
	}
	if len(recs) == 0 || !slices.Equal(recs[0], header) {
		return nil, errors.New("recon: missing or unexpected header")
	}
	rows := make([]Row, 0, len(recs)-1)
	for i, rec := range recs[1:] {
		row, err := parseRow(rec)
		if err != nil {
			return nil, fmt.Errorf("recon: line %d: %w", i+2, err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func parseRow(rec []string) (Row, error) {
	amount, err := strconv.ParseInt(rec[2], 10, 64)
	if err != nil {
		return Row{}, err
	}
	d, err := navdate.ParseDate(rec[3])
	if err != nil {
		return Row{}, err
	}
	nav, err := money.ParseNAV(rec[4])
	if err != nil {
		return Row{}, err
	}
	units, err := money.ParseUnits(rec[5])
	if err != nil {
		return Row{}, err
	}
	return Row{Ref: rec[0], SchemeCode: rec[1], AmountPaise: amount, NAVDate: d, NAV: nav, Units: units}, nil
}

// Reconciler checks allotment files against orders.
type Reconciler struct {
	DB       *store.DB
	Rules    navdate.Rules
	Calendar navdate.Calendar
}

// Run applies one allotment file. fileID names the file; a file run twice changes nothing
// the second time. Paid orders that are still not allotted after the file, and were paid
// before missingBefore, get a missing_allotment finding.
func (rc Reconciler) Run(ctx context.Context, fileID string, rows []Row, missingBefore time.Time) (Report, error) {
	rep := Report{Rows: len(rows), Outcomes: map[string]int{}}
	for _, row := range rows {
		outcome, err := rc.applyRow(ctx, fileID, row)
		if err != nil {
			return rep, fmt.Errorf("recon: %s: %w", row.Ref, err)
		}
		rep.Outcomes[outcome]++
	}
	open, err := rc.DB.OrdersIn(ctx, order.Submitted, order.SubmitUncertain, order.Accepted)
	if err != nil {
		return rep, err
	}
	for _, o := range open {
		if o.PaidAt.IsZero() || !o.PaidAt.Before(missingBefore) {
			continue
		}
		err := rc.DB.WithTx(ctx, func(tx *store.Tx) error {
			return tx.RecordFinding(ctx, o.ID, MissingAllotment, fmt.Sprintf("paid at %s, state %s, no row in %s", o.PaidAt.UTC().Format(time.RFC3339), o.State, fileID))
		})
		if err != nil {
			return rep, err
		}
		rep.NewMissing++
	}
	return rep, nil
}

func (rc Reconciler) applyRow(ctx context.Context, fileID string, row Row) (string, error) {
	payload, err := json.Marshal(row)
	if err != nil {
		return "", err
	}
	id, idErr := uuid.Parse(strings.TrimPrefix(row.Ref, "ALT"))
	if idErr != nil || !strings.HasPrefix(row.Ref, "ALT") {
		id = uuid.UUID{}
	}
	var o order.Order
	if id != (uuid.UUID{}) {
		o, err = rc.DB.Order(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			id = uuid.UUID{}
		} else if err != nil {
			return "", err
		}
	}
	src := store.Source{Name: "rta", EventID: fileID + ":" + row.Ref}
	var outcome string
	err = rc.DB.WithTx(ctx, func(tx *store.Tx) error {
		fresh, err := tx.RecordInbound(ctx, src.Name, src.EventID, id, "rta.allotted", payload)
		if err != nil || !fresh {
			outcome = "duplicate"
			return err
		}
		if id == (uuid.UUID{}) {
			outcome = UnknownReference
			return tx.SetInboundOutcome(ctx, src.Name, src.EventID, outcome)
		}
		if kind, detail := rc.check(o, row); kind != "" {
			outcome = kind
			if err := tx.RecordFinding(ctx, id, kind, detail); err != nil {
				return err
			}
			return tx.SetInboundOutcome(ctx, src.Name, src.EventID, outcome)
		}
		outcome, err = tx.ApplyOrFlag(ctx, id, order.AllotmentSeen{NAVDate: row.NAVDate, NAV: row.NAV, Units: row.Units}, src)
		if err != nil {
			return err
		}
		return tx.SetInboundOutcome(ctx, src.Name, src.EventID, outcome)
	})
	return outcome, err
}

// check returns a finding kind and detail when a row disagrees with the order, or "".
func (rc Reconciler) check(o order.Order, row Row) (string, string) {
	if row.AmountPaise != int64(o.Amount) {
		return UnitMismatch, fmt.Sprintf("file amount %d paise, order amount %d paise", row.AmountPaise, o.Amount)
	}
	want, err := money.UnitsFor(o.Amount, row.NAV)
	if err != nil || want != row.Units {
		return UnitMismatch, fmt.Sprintf("file units %s, amount %s at NAV %s buys %s", row.Units, o.Amount, row.NAV, want)
	}
	if o.PaidAt.IsZero() {
		return NAVDateMismatch, "allotment for an order with no recorded payment"
	}
	d, err := rc.Rules.ForPurchase(o.Category, o.CreatedAt, o.PaidAt, rc.Calendar)
	if err != nil {
		return NAVDateMismatch, err.Error()
	}
	if d.NAVDate != row.NAVDate {
		return NAVDateMismatch, fmt.Sprintf("file NAV date %s, rules give %s", row.NAVDate, d.NAVDate)
	}
	return "", ""
}

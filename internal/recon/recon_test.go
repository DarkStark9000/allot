package recon_test

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/DarkStark9000/allot/internal/exchange"
	"github.com/DarkStark9000/allot/internal/fake"
	"github.com/DarkStark9000/allot/internal/money"
	"github.com/DarkStark9000/allot/internal/navdate"
	"github.com/DarkStark9000/allot/internal/order"
	"github.com/DarkStark9000/allot/internal/recon"
	"github.com/DarkStark9000/allot/internal/store"
	"github.com/DarkStark9000/allot/internal/testdb"
)

var (
	cal     = navdate.NewCalendar()
	ordered = time.Date(2026, time.October, 9, 11, 0, 0, 0, navdate.IST)
)

// paid stores an order and its payment, then applies extra events.
func paid(t *testing.T, db *store.DB, events ...order.Event) order.Order {
	t.Helper()
	o, err := order.New(uuid.NewV7(), "u", "INF000K01ABC", navdate.Other, 5_000_00, ordered)
	if err != nil {
		t.Fatal(err)
	}
	all := append([]order.Event{order.PaymentSucceeded{PaymentID: "pay", At: ordered.Add(time.Minute)}}, events...)
	err = db.WithTx(t.Context(), func(tx *store.Tx) error {
		if err := tx.InsertOrder(t.Context(), o, o.ID.String()); err != nil {
			return err
		}
		for _, e := range all {
			if _, err := tx.Apply(t.Context(), o.ID, e, store.Source{Name: "test", EventID: e.Kind()}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	o, _ = db.Order(t.Context(), o.ID)
	return o
}

// fileFor builds an allotment file with the fake registrar, as the real one would see it.
func fileFor(t *testing.T, f fake.RTAFaults, orders ...order.Order) []recon.Row {
	t.Helper()
	var ex []fake.ExchangeOrder
	for _, o := range orders {
		ex = append(ex, fake.ExchangeOrder{Status: exchange.Accepted, SubmitRequest: exchange.SubmitRequest{
			Ref: o.ExchangeRef(), SchemeCode: o.SchemeCode, Category: string(o.Category),
			AmountPaise: int64(o.Amount), OrderedAt: o.CreatedAt, FundsRealisedAt: o.PaidAt,
		}})
	}
	b, err := fake.AllotmentFile(ex, navdate.Default(), cal, 1, f)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := recon.ParseFile(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func run(t *testing.T, db *store.DB, fileID string, rows []recon.Row) recon.Report {
	t.Helper()
	rc := recon.Reconciler{DB: db, Rules: navdate.Default(), Calendar: cal}
	rep, err := rc.Run(t.Context(), fileID, rows, ordered.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func findings(t *testing.T, db *store.DB) map[string]int {
	t.Helper()
	fs, err := db.Findings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, f := range fs {
		out[f.Kind]++
	}
	return out
}

func TestRecon_Allots(t *testing.T) {
	db := testdb.New(t)
	o := paid(t, db, order.ExchangeAccepted{})
	run(t, db, "day1", fileFor(t, fake.RTAFaults{}, o))
	got, _ := db.Order(t.Context(), o.ID)
	if got.State != order.Allotted || got.NAVDate != (navdate.Date{Year: 2026, Month: time.October, Day: 9}) {
		t.Fatalf("order = %s on %s, want allotted on 2026-10-09", got.State, got.NAVDate)
	}
	want, _ := money.UnitsFor(o.Amount, got.NAV)
	if got.Units != want {
		t.Errorf("units = %s, want %s", got.Units, want)
	}
	if rep := run(t, db, "day1", fileFor(t, fake.RTAFaults{}, o)); rep.Outcomes["duplicate"] != 1 {
		t.Errorf("same file again: %v, want one duplicate", rep.Outcomes)
	}
}

// F9: the allotment arrives before the exchange's acceptance. Units prove acceptance.
func TestAllotment_EarlyEvidence(t *testing.T) {
	db := testdb.New(t)
	o := paid(t, db, order.ExchangeTimedOut{})
	run(t, db, "day1", fileFor(t, fake.RTAFaults{}, o))
	if got, _ := db.Order(t.Context(), o.ID); got.State != order.Allotted {
		t.Fatalf("state = %s, want allotted", got.State)
	}
}

// F11: a paid order has no row in the file. Reconciliation flags it after the deadline.
func TestRecon_MissingAllotment(t *testing.T) {
	db := testdb.New(t)
	o := paid(t, db, order.ExchangeAccepted{})
	rep := run(t, db, "day1", fileFor(t, fake.RTAFaults{DropRow: 1}, o))
	if rep.NewMissing != 1 || findings(t, db)[recon.MissingAllotment] != 1 {
		t.Fatalf("report %+v, findings %v; want one missing allotment", rep, findings(t, db))
	}
}

// F12: the units in the file do not match amount over NAV. The row is not applied.
func TestRecon_UnitMismatch(t *testing.T) {
	db := testdb.New(t)
	o := paid(t, db, order.ExchangeAccepted{})
	run(t, db, "day1", fileFor(t, fake.RTAFaults{WrongUnits: 1}, o))
	if got, _ := db.Order(t.Context(), o.ID); got.State != order.Accepted {
		t.Errorf("state = %s, want accepted: a wrong row must not allot", got.State)
	}
	if findings(t, db)[recon.UnitMismatch] != 1 {
		t.Errorf("findings = %v, want one unit mismatch", findings(t, db))
	}
}

func TestRecon_NAVDateMismatch(t *testing.T) {
	db := testdb.New(t)
	o := paid(t, db, order.ExchangeAccepted{})
	rows := fileFor(t, fake.RTAFaults{}, o)
	rows[0].NAVDate = rows[0].NAVDate.AddDays(1)
	run(t, db, "day1", rows)
	if findings(t, db)[recon.NAVDateMismatch] != 1 {
		t.Errorf("findings = %v, want one NAV date mismatch", findings(t, db))
	}
}

func TestRecon_UnknownReference(t *testing.T) {
	db := testdb.New(t)
	rows := []recon.Row{{Ref: "ALT" + uuid.NewV7().String(), AmountPaise: 100, NAV: 10_0000, Units: 100}}
	if rep := run(t, db, "day1", rows); rep.Outcomes[recon.UnknownReference] != 1 {
		t.Errorf("outcomes = %v", rep.Outcomes)
	}
}

func TestParseFileRejectsBadInput(t *testing.T) {
	bad := []string{
		"",
		"ref,scheme\nx,y\n",
		strings.Join(fake.AllotmentHeader, ",") + "\nALT1,S,abc,2026-10-09,10.0000,1.000\n",
		strings.Join(fake.AllotmentHeader, ",") + "\nALT1,S,100,2026-13-09,10.0000,1.000\n",
	}
	for _, in := range bad {
		if _, err := recon.ParseFile(strings.NewReader(in)); err == nil {
			t.Errorf("ParseFile(%q): want error", in)
		}
	}
}

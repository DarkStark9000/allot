package fake

import (
	"bytes"
	"encoding/csv"
	"hash/fnv"
	"math/rand/v2"
	"strconv"

	"github.com/DarkStark9000/allot/internal/exchange"
	"github.com/DarkStark9000/allot/internal/money"
	"github.com/DarkStark9000/allot/internal/navdate"
)

// RTAFaults are the chances, from 0 to 1, that a row in the allotment file is wrong.
type RTAFaults struct {
	// DropRow: the row is missing.
	DropRow float64
	// WrongUnits: the row's units do not match amount over NAV.
	WrongUnits float64
}

// AllotmentHeader is the first row of an allotment file.
var AllotmentHeader = []string{"exchange_ref", "scheme_code", "amount_paise", "nav_date", "nav", "units"}

// NAVFor returns the fake NAV of a scheme on a date: a stable value between 10 and 100.
func NAVFor(scheme string, d navdate.Date) money.NAV {
	h := fnv.New64a()
	h.Write([]byte(scheme + "|" + d.String()))
	return money.NAV(10_0000 + h.Sum64()%90_0000)
}

// AllotmentFile writes the registrar's end-of-day file for the accepted orders, as CSV.
func AllotmentFile(orders []ExchangeOrder, rules navdate.Rules, cal navdate.Calendar, seed uint64, f RTAFaults) ([]byte, error) {
	rnd := rand.New(rand.NewPCG(seed, 0x525441))
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(AllotmentHeader); err != nil {
		return nil, err
	}
	for _, o := range orders {
		if o.Status != exchange.Accepted {
			continue
		}
		if rnd.Float64() < f.DropRow {
			continue
		}
		d, err := rules.ForPurchase(navdate.Category(o.Category), o.OrderedAt, o.FundsRealisedAt, cal)
		if err != nil {
			return nil, err
		}
		nav := NAVFor(o.SchemeCode, d.NAVDate)
		units, err := money.UnitsFor(money.Paise(o.AmountPaise), nav)
		if err != nil {
			return nil, err
		}
		if rnd.Float64() < f.WrongUnits {
			units += money.Units(1 + rnd.IntN(500))
		}
		err = w.Write([]string{o.Ref, o.SchemeCode, strconv.FormatInt(o.AmountPaise, 10), d.NAVDate.String(), nav.String(), units.String()})
		if err != nil {
			return nil, err
		}
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}

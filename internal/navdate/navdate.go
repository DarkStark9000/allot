// Package navdate decides which day's net asset value (NAV) applies to a purchase.
//
// The rules are data, embedded from rules.json, and each rule names its source.
// A rule change is a data change with a test, never a constant in code.
package navdate

import (
	_ "embed"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"time"
)

// IST is India Standard Time, the zone every cut-off is stated in.
var IST = time.FixedZone("IST", 5*60*60+30*60)

// Date is a calendar date in India Standard Time.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// DateOf returns the date of t in India Standard Time.
func DateOf(t time.Time) Date {
	t = t.In(IST)
	return Date{t.Year(), t.Month(), t.Day()}
}

// ParseDate parses a date in the form 2006-01-02.
func ParseDate(s string) (Date, error) {
	t, err := time.ParseInLocation(time.DateOnly, s, IST)
	if err != nil {
		return Date{}, fmt.Errorf("navdate: %w", err)
	}
	return DateOf(t), nil
}

func (d Date) String() string { return d.at().Format(time.DateOnly) }

// AddDays returns the date n calendar days after d. A negative n moves back.
func (d Date) AddDays(n int) Date { return DateOf(d.at().AddDate(0, 0, n)) }

// Weekday returns the day of the week of d.
func (d Date) Weekday() time.Weekday { return d.at().Weekday() }

// Compare returns -1, 0, or 1 as d is before, equal to, or after o.
func (d Date) Compare(o Date) int { return d.at().Compare(o.at()) }

// MarshalText encodes d as 2006-01-02.
func (d Date) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalText decodes a date in the form 2006-01-02.
func (d *Date) UnmarshalText(b []byte) error {
	v, err := ParseDate(string(b))
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// at returns noon on d, which is safe from any daylight or zone edge.
func (d Date) at() time.Time { return time.Date(d.Year, d.Month, d.Day, 12, 0, 0, 0, IST) }

// Calendar knows which dates are business days.
type Calendar struct {
	holidays map[Date]bool
}

// NewCalendar returns a calendar with the given holidays. Saturdays and Sundays are never business days.
func NewCalendar(holidays ...Date) Calendar {
	c := Calendar{holidays: make(map[Date]bool, len(holidays))}
	for _, h := range holidays {
		c.holidays[h] = true
	}
	return c
}

// IsBusinessDay reports whether d is a business day.
func (c Calendar) IsBusinessDay(d Date) bool {
	switch d.Weekday() {
	case time.Saturday, time.Sunday:
		return false
	}
	return !c.holidays[d]
}

// NextBusinessDay returns the first business day after d.
func (c Calendar) NextBusinessDay(d Date) Date {
	for d = d.AddDays(1); !c.IsBusinessDay(d); d = d.AddDays(1) {
	}
	return d
}

// Category is a scheme category, as far as NAV applicability is concerned.
type Category string

// Liquid and overnight schemes have their own cut-off; every other scheme shares one.
const (
	Liquid    Category = "liquid"
	Overnight Category = "overnight"
	Other     Category = "other"
)

// Basis names which day's NAV applies.
type Basis string

// The bases a rule can name, relative to the business day the purchase counts as received.
const (
	ReceiptDay               Basis = "receipt_day"
	DayBeforeReceipt         Basis = "day_before_receipt"
	DayBeforeNextBusinessDay Basis = "day_before_next_business_day"
	NextBusinessDay          Basis = "next_business_day"
)

var bases = []Basis{ReceiptDay, DayBeforeReceipt, DayBeforeNextBusinessDay, NextBusinessDay}

// Rule says which NAV applies to purchases in some categories, before and after a cut-off.
type Rule struct {
	Categories    []Category `json:"categories"`
	Cutoff        string     `json:"cutoff"`
	BeforeCutoff  Basis      `json:"before_cutoff"`
	AfterCutoff   Basis      `json:"after_cutoff"`
	EffectiveFrom Date       `json:"effective_from"`
	Source        string     `json:"source"`

	cutoffSeconds int
}

// Rules is a validated rule set that covers every category exactly once.
type Rules struct {
	byCategory map[Category]Rule
}

//go:embed rules.json
var rulesJSON []byte

var defaultRules = mustLoad(rulesJSON)

// Default returns the embedded rule set.
func Default() Rules { return defaultRules }

// Load parses and validates a rule set.
func Load(data []byte) (Rules, error) {
	var list []Rule
	if err := json.Unmarshal(data, &list, json.RejectUnknownMembers(true)); err != nil {
		return Rules{}, fmt.Errorf("navdate: rules: %w", err)
	}
	rs := Rules{byCategory: make(map[Category]Rule)}
	for i, r := range list {
		var h, m int
		if _, err := fmt.Sscanf(r.Cutoff, "%02d:%02d", &h, &m); err != nil || h > 23 || m > 59 || len(r.Cutoff) != 5 {
			return Rules{}, fmt.Errorf("navdate: rule %d: cut-off %q is not HH:MM", i, r.Cutoff)
		}
		r.cutoffSeconds = h*3600 + m*60
		if !slices.Contains(bases, r.BeforeCutoff) || !slices.Contains(bases, r.AfterCutoff) {
			return Rules{}, fmt.Errorf("navdate: rule %d: unknown basis", i)
		}
		if r.Source == "" {
			return Rules{}, fmt.Errorf("navdate: rule %d: no source", i)
		}
		for _, c := range r.Categories {
			if _, dup := rs.byCategory[c]; dup {
				return Rules{}, fmt.Errorf("navdate: category %q has two rules", c)
			}
			rs.byCategory[c] = r
		}
	}
	for _, c := range []Category{Liquid, Overnight, Other} {
		if _, ok := rs.byCategory[c]; !ok {
			return Rules{}, fmt.Errorf("navdate: category %q has no rule", c)
		}
	}
	return rs, nil
}

func mustLoad(data []byte) Rules {
	rs, err := Load(data)
	if err != nil {
		panic(err)
	}
	return rs
}

// ErrUnknownCategory reports a category with no rule.
var ErrUnknownCategory = errors.New("navdate: unknown category")

// Decision is the applicable NAV date and the reason for it.
type Decision struct {
	NAVDate      Date
	Received     time.Time
	BeforeCutoff bool
	Rule         Rule
}

// ForPurchase returns the NAV date for a purchase. The purchase counts as received when
// both the order and the money have reached the fund, whichever is later. A purchase
// received on a non-business day counts as received at the start of the next business day.
func (rs Rules) ForPurchase(c Category, ordered, realised time.Time, cal Calendar) (Decision, error) {
	rule, ok := rs.byCategory[c]
	if !ok {
		return Decision{}, fmt.Errorf("%w: %q", ErrUnknownCategory, c)
	}
	received := ordered
	if realised.After(received) {
		received = realised
	}
	local := received.In(IST)
	day := DateOf(local)
	before := true
	if cal.IsBusinessDay(day) {
		h, m, s := local.Clock()
		before = h*3600+m*60+s <= rule.cutoffSeconds
	} else {
		day = cal.NextBusinessDay(day)
	}
	basis := rule.AfterCutoff
	if before {
		basis = rule.BeforeCutoff
	}
	return Decision{NAVDate: resolve(basis, day, cal), Received: received, BeforeCutoff: before, Rule: rule}, nil
}

func resolve(b Basis, day Date, cal Calendar) Date {
	switch b {
	case DayBeforeReceipt:
		return day.AddDays(-1)
	case DayBeforeNextBusinessDay:
		return cal.NextBusinessDay(day).AddDays(-1)
	case NextBusinessDay:
		return cal.NextBusinessDay(day)
	default:
		return day
	}
}

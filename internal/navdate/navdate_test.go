package navdate

import (
	"errors"
	"testing"
	"time"
)

// 2 October 2026 (Gandhi Jayanti) is a Friday holiday in this test calendar.
var testCal = NewCalendar(Date{2026, time.October, 2})

func at(day, hh, mm, ss int) time.Time {
	return time.Date(2026, time.October, day, hh, mm, ss, 0, IST)
}

func d(day int) Date { return Date{2026, time.October, day} }

func TestNAVDate(t *testing.T) {
	tests := []struct {
		name     string
		category Category
		ordered  time.Time
		realised time.Time
		want     Date
		before   bool
	}{
		{"other: money before cut-off", Other, at(9, 14, 0, 0), at(9, 14, 10, 0), d(9), true},
		{"other: exactly at cut-off counts", Other, at(9, 14, 0, 0), at(9, 15, 0, 0), d(9), true},
		{"other: one second late", Other, at(9, 14, 0, 0), at(9, 15, 0, 1), d(12), false},
		{"other: order early, money late", Other, at(9, 10, 0, 0), at(9, 15, 30, 0), d(12), false},
		{"other: money early, order late", Other, at(9, 16, 0, 0), at(9, 10, 0, 0), d(12), false},
		{"other: Saturday", Other, at(10, 11, 0, 0), at(10, 11, 0, 0), d(12), true},
		{"other: holiday", Other, at(2, 10, 0, 0), at(2, 10, 0, 0), d(5), true},
		{"other: after cut-off before a holiday", Other, at(1, 16, 0, 0), at(1, 16, 0, 0), d(5), false},
		{"liquid: before cut-off", Liquid, at(9, 13, 0, 0), at(9, 13, 0, 0), d(8), true},
		{"liquid: after cut-off on Friday", Liquid, at(9, 14, 0, 0), at(9, 14, 0, 0), d(11), false},
		{"liquid: after cut-off before a holiday", Liquid, at(1, 14, 0, 0), at(1, 14, 0, 0), d(4), false},
		{"liquid: Sunday", Liquid, at(11, 9, 0, 0), at(11, 9, 0, 0), d(11), true},
		{"overnight follows liquid", Overnight, at(9, 13, 31, 0), at(9, 13, 31, 0), d(11), false},
		{"other: UTC input", Other, time.Date(2026, time.October, 9, 9, 29, 0, 0, time.UTC), at(9, 9, 0, 0), d(9), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Default().ForPurchase(tt.category, tt.ordered, tt.realised, testCal)
			if err != nil {
				t.Fatal(err)
			}
			if got.NAVDate != tt.want || got.BeforeCutoff != tt.before {
				t.Errorf("NAV date = %s (before cut-off %t), want %s (%t)", got.NAVDate, got.BeforeCutoff, tt.want, tt.before)
			}
			if got.Rule.Source == "" {
				t.Error("decision has no rule source")
			}
		})
	}
}

func TestUnknownCategory(t *testing.T) {
	_, err := Default().ForPurchase("gold", at(9, 10, 0, 0), at(9, 10, 0, 0), testCal)
	if !errors.Is(err, ErrUnknownCategory) {
		t.Fatalf("got %v, want ErrUnknownCategory", err)
	}
}

func TestLoadRejectsBadRules(t *testing.T) {
	bad := map[string]string{
		"missing category": `[{"categories":["liquid","overnight"],"cutoff":"13:30","before_cutoff":"receipt_day","after_cutoff":"next_business_day","effective_from":"2021-02-01","source":"x"}]`,
		"bad cut-off":      `[{"categories":["liquid","overnight","other"],"cutoff":"3pm","before_cutoff":"receipt_day","after_cutoff":"next_business_day","effective_from":"2021-02-01","source":"x"}]`,
		"unknown basis":    `[{"categories":["liquid","overnight","other"],"cutoff":"15:00","before_cutoff":"tomorrow","after_cutoff":"next_business_day","effective_from":"2021-02-01","source":"x"}]`,
		"no source":        `[{"categories":["liquid","overnight","other"],"cutoff":"15:00","before_cutoff":"receipt_day","after_cutoff":"next_business_day","effective_from":"2021-02-01","source":""}]`,
		"duplicate":        `[{"categories":["liquid","overnight","other","other"],"cutoff":"15:00","before_cutoff":"receipt_day","after_cutoff":"next_business_day","effective_from":"2021-02-01","source":"x"}]`,
		"unknown field":    `[{"categories":["liquid","overnight","other"],"cutoff":"15:00","before_cutoff":"receipt_day","after_cutoff":"next_business_day","effective_from":"2021-02-01","source":"x","note":1}]`,
	}
	for name, data := range bad {
		if _, err := Load([]byte(data)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestDateText(t *testing.T) {
	var got Date
	if err := got.UnmarshalText([]byte("2026-10-09")); err != nil || got != d(9) {
		t.Fatalf("UnmarshalText = %v, %v", got, err)
	}
	if b, _ := got.MarshalText(); string(b) != "2026-10-09" {
		t.Errorf("MarshalText = %s", b)
	}
	if d(9).Compare(d(12)) != -1 || d(12).Compare(d(9)) != 1 || d(9).Compare(d(9)) != 0 {
		t.Error("Compare is wrong")
	}
}

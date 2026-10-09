package money

import (
	"errors"
	"testing"
)

func TestUnitsFor(t *testing.T) {
	tests := []struct {
		name   string
		amount Paise
		nav    NAV
		want   Units
	}{
		{"exact", 1_000_00, 10_0000, 100_000},          // 1000 rupees at 10.0000 is 100.000 units
		{"truncates", 5_000_00, 61_7234, 81_006},       // 5000 / 61.7234 = 81.00655..., truncated
		{"one rupee at high NAV", 1_00, 4_512_1234, 0}, // less than a thousandth of a unit
		{"large amount", 10_000_000_00, 1_0000, 10_000_000_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := UnitsFor(tt.amount, tt.nav)
			if err != nil {
				t.Fatalf("UnitsFor(%s, %s): %v", tt.amount, tt.nav, err)
			}
			if got != tt.want {
				t.Errorf("UnitsFor(%s, %s) = %s, want %s", tt.amount, tt.nav, got, tt.want)
			}
		})
	}
}

func TestUnitsForRejects(t *testing.T) {
	if _, err := UnitsFor(0, 10_0000); err == nil {
		t.Error("zero amount: want error")
	}
	if _, err := UnitsFor(100, 0); err == nil {
		t.Error("zero NAV: want error")
	}
	if _, err := UnitsFor(Paise(1<<62), 1); !errors.Is(err, ErrOverflow) {
		t.Errorf("huge amount: got %v, want ErrOverflow", err)
	}
}

func TestParseAndFormat(t *testing.T) {
	tests := []struct {
		in   string
		nav  NAV
		text string
	}{
		{"123.4567", 123_4567, "123.4567"},
		{"10", 10_0000, "10.0000"},
		{"0.5", 5000, "0.5000"},
		{" 61.72 ", 61_7200, "61.7200"},
	}
	for _, tt := range tests {
		got, err := ParseNAV(tt.in)
		if err != nil {
			t.Fatalf("ParseNAV(%q): %v", tt.in, err)
		}
		if got != tt.nav || got.String() != tt.text {
			t.Errorf("ParseNAV(%q) = %d (%s), want %d (%s)", tt.in, got, got, tt.nav, tt.text)
		}
	}
	for _, bad := range []string{"", "-1", "+1", "1.23456", "abc", "1.-2", ".5"} {
		if _, err := ParseNAV(bad); err == nil {
			t.Errorf("ParseNAV(%q): want error", bad)
		}
	}
	if got := Paise(-1234_56).String(); got != "-1234.56" {
		t.Errorf("Paise.String() = %q", got)
	}
	if got, _ := ParseUnits("81.004"); got != 81_004 {
		t.Errorf("ParseUnits = %d", got)
	}
}

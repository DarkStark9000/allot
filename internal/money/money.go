// Package money holds fixed-point types for rupees, net asset values, and fund units.
// Nothing in the money path uses floating point.
package money

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Paise is an amount in Indian rupees, counted in paise (100 paise = 1 rupee).
type Paise int64

// String formats p as rupees with two decimals, such as "1234.50".
func (p Paise) String() string { return formatFixed(int64(p), 2) }

// NAV is a net asset value per unit, counted in ten-thousandths of a rupee.
type NAV int64

// String formats n with four decimals, such as "123.4567".
func (n NAV) String() string { return formatFixed(int64(n), 4) }

// Units is a quantity of fund units, counted in thousandths of a unit.
type Units int64

// String formats u with three decimals, such as "81.004".
func (u Units) String() string { return formatFixed(int64(u), 3) }

// ErrOverflow reports an amount too large to convert without overflow.
var ErrOverflow = errors.New("money: amount too large")

// paiseToMilliUnits converts paise at a NAV in ten-thousandths of a rupee into
// thousandths of a unit: units = (amount/100) / (nav/10000), times 1000.
const paiseToMilliUnits = 100_000

// UnitsFor returns the units that amount buys at nav, truncated to a thousandth of a unit.
func UnitsFor(amount Paise, nav NAV) (Units, error) {
	if amount <= 0 {
		return 0, fmt.Errorf("money: amount must be positive, got %s", amount)
	}
	if nav <= 0 {
		return 0, fmt.Errorf("money: NAV must be positive, got %s", nav)
	}
	if int64(amount) > math.MaxInt64/paiseToMilliUnits {
		return 0, ErrOverflow
	}
	return Units(int64(amount) * paiseToMilliUnits / int64(nav)), nil
}

// ParseNAV parses a decimal string with at most four decimals, such as "123.4567".
func ParseNAV(s string) (NAV, error) {
	v, err := parseFixed(s, 4)
	return NAV(v), err
}

// ParseUnits parses a decimal string with at most three decimals, such as "81.004".
func ParseUnits(s string) (Units, error) {
	v, err := parseFixed(s, 3)
	return Units(v), err
}

func formatFixed(v int64, scale int) string {
	sign := ""
	if v < 0 {
		sign = "-"
		v = -v
	}
	div := pow10(scale)
	return fmt.Sprintf("%s%d.%0*d", sign, v/div, scale, v%div)
}

func parseFixed(s string, scale int) (int64, error) {
	whole, frac, _ := strings.Cut(strings.TrimSpace(s), ".")
	if whole == "" || strings.HasPrefix(whole, "-") || strings.HasPrefix(whole, "+") {
		return 0, fmt.Errorf("money: invalid decimal %q", s)
	}
	if len(frac) > scale {
		return 0, fmt.Errorf("money: %q has more than %d decimals", s, scale)
	}
	frac += strings.Repeat("0", scale-len(frac))
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("money: invalid decimal %q: %w", s, err)
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil || (frac != "" && frac[0] == '-') {
		return 0, fmt.Errorf("money: invalid decimal %q", s)
	}
	div := pow10(scale)
	if w > (math.MaxInt64-f)/div {
		return 0, ErrOverflow
	}
	return w*div + f, nil
}

func pow10(n int) int64 {
	v := int64(1)
	for range n {
		v *= 10
	}
	return v
}

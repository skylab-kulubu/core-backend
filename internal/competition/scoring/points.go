// Package scoring computes a Stage's standings from its ledger (ADR-0068).
// A Scoring strategy is a pure function: no I/O, no clock. The ranker turns
// standings into ranks the same way for every strategy.
package scoring

import (
	"errors"
	"strconv"
	"strings"
)

// Points is a score in thousandths of a point: fixed point, so equal scores
// compare equal and no floating-point rounding decides a tie (spec §5.10).
// In JSON it is a plain number with at most three decimals (30, 50.5,
// 0.125).
type Points int64

// Scale is how many Points make one point.
const Scale = 1000

// MaxPoints bounds one ledger value either way: a billion points.
const MaxPoints Points = 1_000_000_000 * Scale

// ErrInvalidPoints is a value that is not a decimal number within
// ±MaxPoints with at most three decimals.
var ErrInvalidPoints = errors.New("scoring: points must be a number within ±1000000000 with at most three decimals")

// ParsePoints reads a decimal such as "30", "-2.5" or "0.125". No sign but
// a leading minus, no exponent, at most three decimals.
func ParsePoints(raw string) (Points, error) {
	s := raw
	negative := strings.HasPrefix(s, "-")
	if negative {
		s = s[1:]
	}
	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" || (hasFrac && (frac == "" || len(frac) > 3)) || !digits(whole) || !digits(frac) || len(whole) > 10 {
		return 0, ErrInvalidPoints
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, ErrInvalidPoints
	}
	var f int64
	if frac != "" {
		f, _ = strconv.ParseInt(frac+strings.Repeat("0", 3-len(frac)), 10, 64)
	}
	p := Points(w*Scale + f)
	if p > MaxPoints {
		return 0, ErrInvalidPoints
	}
	if negative {
		p = -p
	}
	return p, nil
}

func digits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// String writes p as a decimal without trailing zeros: 30, 50.5, -0.125.
func (p Points) String() string {
	v := int64(p)
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	out := sign + strconv.FormatInt(v/Scale, 10)
	if frac := v % Scale; frac != 0 {
		out += "." + strings.TrimRight(strconv.FormatInt(frac+Scale, 10)[1:], "0")
	}
	return out
}

func (p Points) MarshalJSON() ([]byte, error) {
	return []byte(p.String()), nil
}

// UnmarshalJSON takes a JSON number only: a string, null or an exponent is
// refused, so a typo cannot become a score.
func (p *Points) UnmarshalJSON(raw []byte) error {
	got, err := ParsePoints(string(raw))
	if err != nil {
		return err
	}
	*p = got
	return nil
}

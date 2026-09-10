package market_test

import (
	"errors"
	"math"
	"math/rand"
	"strconv"
	"testing"

	"praxis/internal/market"
)

// Scenario: the two directions are exact inverses, and the rule is total
//
//	Given any integer the domain can hold
//	Then parsing what it formats returns it, formatting what it parses returns
//	  the same spelling, and every other spelling of the same number is refused.
//
// A test that only checked "the two writers agree" could not fail: both are one
// call to strconv.FormatInt. The rule is in the reading — no leading zero, no
// sign on a positive, no negative zero, no empty string — and a second reader
// written for another boundary is a second policy, which is what this exists to
// prevent. The writer lives beside it because the inverse property is the only
// way to state that the two cannot drift, and it cannot be stated apart.
func TestCanonicalIntegersRoundTripBothWays(t *testing.T) {
	t.Run("parse of format is the identity", func(t *testing.T) {
		r := rand.New(rand.NewSource(1))
		values := []int64{0, 1, -1, 9, 10, -10, math.MaxInt64, math.MinInt64}
		for i := 0; i < 200; i++ {
			values = append(values, r.Int63()-r.Int63())
		}
		for _, v := range values {
			got, err := market.ParseInt(market.FormatInt(v))
			if err != nil {
				t.Fatalf("ParseInt(FormatInt(%d)): %v", v, err)
			}
			if got != v {
				t.Fatalf("round trip: got %d, want %d", got, v)
			}
		}
	})

	t.Run("format of parse is the same spelling", func(t *testing.T) {
		for _, s := range []string{"0", "1", "-1", "10", "-10", "9223372036854775807", "-9223372036854775808"} {
			v, err := market.ParseInt(s)
			if err != nil {
				t.Fatalf("ParseInt(%q): %v", s, err)
			}
			if got := market.FormatInt(v); got != s {
				t.Fatalf("format of parse: got %q, want %q", got, s)
			}
		}
	})

	// Whole values, not characters embedded in one. The last test written this
	// way compared "a"+r+"b" and missed the single value the two gates
	// disagreed on, which is exactly the shape of the mistake being avoided.
	t.Run("every other spelling is refused", func(t *testing.T) {
		for _, s := range []string{
			"", " ", "  ", "+1", " 1", "1 ", "-0", "00", "01", "020000",
			"0x10", "1.0", "1,0", "1e3", "--1", "-", "abc", "1_000",
			"9223372036854775808",  // one past MaxInt64
			"-9223372036854775809", // one past MinInt64
		} {
			if v, err := market.ParseInt(s); err == nil {
				t.Errorf("ParseInt(%q) accepted it as %d", s, v)
			}
		}
	})

	t.Run("unsigned refuses a sign as well", func(t *testing.T) {
		for _, s := range []string{"-1", "-0", "+1", "", "00", "01"} {
			if v, err := market.ParseUint(s); err == nil {
				t.Errorf("ParseUint(%q) accepted it as %d", s, v)
			}
		}
		for _, v := range []uint64{0, 1, 10, math.MaxUint64} {
			got, err := market.ParseUint(market.FormatUint(v))
			if err != nil || got != v {
				t.Fatalf("round trip %d: got %d, %v", v, got, err)
			}
		}
	})

	t.Run("the refusal names itself", func(t *testing.T) {
		if _, err := market.ParseInt("+1"); !errors.Is(err, market.ErrNotCanonicalInt) {
			t.Fatalf("got %v, want %v", err, market.ErrNotCanonicalInt)
		}
	})

	// And the rule really is narrower than the library's, which is the point:
	// strconv accepts spellings a journal must not hold.
	t.Run("the library accepts what the domain refuses", func(t *testing.T) {
		for _, s := range []string{"+1", "-0", "01"} {
			if _, err := strconv.ParseInt(s, 10, 64); err != nil {
				t.Fatalf("the premise is wrong: strconv refuses %q", s)
			}
			if _, err := market.ParseInt(s); err == nil {
				t.Fatalf("the domain accepts %q too, so it adds nothing", s)
			}
		}
	})
}

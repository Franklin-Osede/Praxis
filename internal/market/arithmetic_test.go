package market_test

import (
	"errors"
	"math"
	"math/big"
	"math/rand"
	"testing"

	"praxis/internal/market"
)

// Every money rule in the repository stands on these four functions. They were
// exercised only through the packages above them, so a wrap that produced a
// plausible number could have passed unnoticed.

func TestMulCents(t *testing.T) {
	tests := []struct {
		name string
		a, b market.Cents
		want market.Cents
		err  error
	}{
		{"zero on the left", 0, math.MaxInt64, 0, nil},
		{"zero on the right", math.MaxInt64, 0, 0, nil},
		{"an ordinary product", 20_000, 50, 1_000_000, nil},
		{"a negative product", -20_000, 50, -1_000_000, nil},
		{"two negatives", -20_000, -50, 1_000_000, nil},
		{"the largest representable", math.MaxInt64, 1, math.MaxInt64, nil},
		{"the smallest representable", math.MinInt64, 1, math.MinInt64, nil},
		{"overflow upward", math.MaxInt64, 2, 0, market.ErrOverflow},
		{"overflow downward", math.MinInt64, 2, 0, market.ErrOverflow},
		{"two large negatives overflow", math.MinInt64 + 1, -2, 0, market.ErrOverflow},
		// The one case a division check alone would miss: negating the most
		// negative integer has no positive counterpart.
		{"the most negative value times minus one", math.MinInt64, -1, 0, market.ErrOverflow},
		{"minus one times the most negative value", -1, math.MinInt64, 0, market.ErrOverflow},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := market.MulCents(tc.a, tc.b)
			if !errors.Is(err, tc.err) {
				t.Fatalf("error: got %v, want %v", err, tc.err)
			}
			if got != tc.want {
				t.Fatalf("result: got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestAddCents(t *testing.T) {
	tests := []struct {
		name string
		a, b market.Cents
		want market.Cents
		err  error
	}{
		{"an ordinary sum", 5_000_000, 100, 5_000_100, nil},
		{"adding a negative", 5_000_000, -100, 4_999_900, nil},
		{"adding zero", math.MaxInt64, 0, math.MaxInt64, nil},
		{"reaching the limit exactly", math.MaxInt64 - 1, 1, math.MaxInt64, nil},
		{"passing the limit", math.MaxInt64, 1, 0, market.ErrOverflow},
		{"reaching the floor exactly", math.MinInt64 + 1, -1, math.MinInt64, nil},
		{"passing the floor", math.MinInt64, -1, 0, market.ErrOverflow},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := market.AddCents(tc.a, tc.b)
			if !errors.Is(err, tc.err) || got != tc.want {
				t.Fatalf("got (%d, %v), want (%d, %v)", got, err, tc.want, tc.err)
			}
		})
	}
}

func TestSubCents(t *testing.T) {
	tests := []struct {
		name string
		a, b market.Cents
		want market.Cents
		err  error
	}{
		{"an ordinary difference", 5_000_000, 100, 4_999_900, nil},
		{"subtracting a negative", 5_000_000, -100, 5_000_100, nil},
		{"reaching the floor exactly", math.MinInt64 + 1, 1, math.MinInt64, nil},
		{"passing the floor", math.MinInt64, 1, 0, market.ErrOverflow},
		{"passing the limit", math.MaxInt64, -1, 0, market.ErrOverflow},
		{"the most negative value from zero", 0, math.MinInt64, 0, market.ErrOverflow},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := market.SubCents(tc.a, tc.b)
			if !errors.Is(err, tc.err) || got != tc.want {
				t.Fatalf("got (%d, %v), want (%d, %v)", got, err, tc.want, tc.err)
			}
		})
	}
}

func TestAddQty(t *testing.T) {
	tests := []struct {
		name string
		a, b market.Qty
		want market.Qty
		err  error
	}{
		{"an ordinary sum", 3, 2, 5, nil},
		{"reducing a position", 3, -5, -2, nil},
		{"passing the limit", math.MaxInt64, 1, 0, market.ErrOverflow},
		{"passing the floor", math.MinInt64, -1, 0, market.ErrOverflow},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := market.AddQty(tc.a, tc.b)
			if !errors.Is(err, tc.err) || got != tc.want {
				t.Fatalf("got (%d, %v), want (%d, %v)", got, err, tc.want, tc.err)
			}
		})
	}
}

// Scenario: ticks become money, or say they cannot
func TestInstrumentMoney(t *testing.T) {
	mnq := market.Instrument{Symbol: "MNQ", CentsPerTick: 50}

	tests := []struct {
		name       string
		instrument market.Instrument
		price      market.Ticks
		qty        market.Qty
		want       market.Cents
		err        error
	}{
		{"two contracts", mnq, 20_000, 2, 2_000_000, nil},
		{"a short position", mnq, 20_000, -2, -2_000_000, nil},
		{"nothing", mnq, 20_000, 0, 0, nil},
		{
			name:       "a tick value that overflows before the quantity is applied",
			instrument: market.Instrument{Symbol: "HUGE", CentsPerTick: math.MaxInt64 / 2},
			price:      3, qty: 1, err: market.ErrOverflow,
		},
		{
			name:       "a quantity that overflows a representable price",
			instrument: market.Instrument{Symbol: "UNIT", CentsPerTick: 1},
			price:      math.MaxInt64 / 2, qty: 3, err: market.ErrOverflow,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.instrument.Money(tc.price, tc.qty)
			if !errors.Is(err, tc.err) || got != tc.want {
				t.Fatalf("got (%d, %v), want (%d, %v)", got, err, tc.want, tc.err)
			}
		})
	}
}

// Property: checked arithmetic never returns a wrapped answer.
//
// The reference is arbitrary-precision arithmetic, which cannot wrap. Whenever
// a checked operation reports success its answer must equal the exact one, and
// whenever the exact answer does not fit it must report failure. Nothing here
// trusts the implementation to agree with itself.
func TestPropertyCheckedArithmeticAgreesWithExactArithmetic(t *testing.T) {
	r := rand.New(rand.NewSource(20240901))
	fits := new(big.Int)
	low, high := big.NewInt(math.MinInt64), big.NewInt(math.MaxInt64)

	interesting := []int64{
		0, 1, -1, 2, -2, 50, -50, 20_000, -20_000,
		math.MaxInt64, math.MinInt64, math.MaxInt64 - 1, math.MinInt64 + 1,
		math.MaxInt64 / 2, math.MinInt64 / 2, 1 << 31, -(1 << 31),
	}
	pick := func() int64 {
		if r.Intn(2) == 0 {
			return interesting[r.Intn(len(interesting))]
		}
		return r.Int63() - r.Int63()
	}

	check := func(op string, a, b int64, got market.Cents, err error, exact *big.Int) {
		t.Helper()
		representable := exact.Cmp(low) >= 0 && exact.Cmp(high) <= 0
		switch {
		case representable && err != nil:
			t.Fatalf("%s(%d, %d): refused %s, which fits", op, a, b, exact)
		case !representable && err == nil:
			t.Fatalf("%s(%d, %d): returned %d for %s, which does not fit", op, a, b, got, exact)
		case representable && int64(got) != exact.Int64():
			t.Fatalf("%s(%d, %d): got %d, exactly %s", op, a, b, got, exact)
		}
	}

	for i := 0; i < 20_000; i++ {
		a, b := pick(), pick()
		bigA, bigB := big.NewInt(a), big.NewInt(b)

		got, err := market.MulCents(market.Cents(a), market.Cents(b))
		check("MulCents", a, b, got, err, fits.Mul(bigA, bigB))

		got, err = market.AddCents(market.Cents(a), market.Cents(b))
		check("AddCents", a, b, got, err, new(big.Int).Add(bigA, bigB))

		got, err = market.SubCents(market.Cents(a), market.Cents(b))
		check("SubCents", a, b, got, err, new(big.Int).Sub(bigA, bigB))

		q, qerr := market.AddQty(market.Qty(a), market.Qty(b))
		check("AddQty", a, b, market.Cents(q), qerr, new(big.Int).Add(bigA, bigB))
	}
}

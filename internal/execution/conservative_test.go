package execution_test

import (
	"errors"
	"math/rand"
	"reflect"
	"testing"

	"praxis/internal/execution"
	"praxis/internal/market"
)

var mnq = market.Instrument{Symbol: "MNQ"}

func mustOrder(t *testing.T, side market.Side, qty market.Qty) market.Order {
	t.Helper()
	o, err := market.NewMarketOrder("o-1", mnq, side, qty)
	if err != nil {
		t.Fatalf("NewMarketOrder: %v", err)
	}
	return o
}

// Scenario: a market order crosses the spread and takes only what is there
//
//	Given a two-sided quote for MNQ
//	When a market order is executed against it
//	Then a buy fills at the ask and a sell at the bid, never better,
//	  and never for more contracts than the taken side displays.
func TestExecuteOnQuoteMarketOrder(t *testing.T) {
	quote := market.Quote{
		Instrument: mnq,
		Time:       1_700_000_000_000_000_000,
		Bid:        20_000,
		Ask:        20_001,
		BidSize:    4,
		AskSize:    3,
	}

	tests := []struct {
		name  string
		order market.Order
		quote market.Quote
		want  []market.Fill
	}{
		{
			name:  "buy fills at the ask",
			order: mustOrder(t, market.SideBuy, 2),
			quote: quote,
			want: []market.Fill{{
				OrderID: "o-1", Instrument: mnq, Time: quote.Time,
				Side: market.SideBuy, Price: 20_001, Qty: 2,
			}},
		},
		{
			name:  "sell fills at the bid",
			order: mustOrder(t, market.SideSell, 2),
			quote: quote,
			want: []market.Fill{{
				OrderID: "o-1", Instrument: mnq, Time: quote.Time,
				Side: market.SideSell, Price: 20_000, Qty: 2,
			}},
		},
		{
			name:  "buy larger than the ask size fills partially",
			order: mustOrder(t, market.SideBuy, 10),
			quote: quote,
			want: []market.Fill{{
				OrderID: "o-1", Instrument: mnq, Time: quote.Time,
				Side: market.SideBuy, Price: 20_001, Qty: 3,
			}},
		},
		{
			name:  "buy exactly the ask size fills in full",
			order: mustOrder(t, market.SideBuy, 3),
			quote: quote,
			want: []market.Fill{{
				OrderID: "o-1", Instrument: mnq, Time: quote.Time,
				Side: market.SideBuy, Price: 20_001, Qty: 3,
			}},
		},
		{
			name:  "no ask size means no fill and no error",
			order: mustOrder(t, market.SideBuy, 2),
			quote: market.Quote{Instrument: mnq, Time: 5, Bid: 20_000, Ask: 20_001, BidSize: 4, AskSize: 0},
			want:  nil,
		},
		{
			name:  "a locked book is tradable",
			order: mustOrder(t, market.SideBuy, 1),
			quote: market.Quote{Instrument: mnq, Time: 7, Bid: 20_000, Ask: 20_000, BidSize: 1, AskSize: 1},
			want: []market.Fill{{
				OrderID: "o-1", Instrument: mnq, Time: 7,
				Side: market.SideBuy, Price: 20_000, Qty: 1,
			}},
		},
		{
			name:  "a sell ignores the ask side entirely",
			order: mustOrder(t, market.SideSell, 10),
			quote: market.Quote{Instrument: mnq, Time: 9, Bid: 19_999, Ask: 20_001, BidSize: 2, AskSize: 100},
			want: []market.Fill{{
				OrderID: "o-1", Instrument: mnq, Time: 9,
				Side: market.SideSell, Price: 19_999, Qty: 2,
			}},
		},
	}

	var policy execution.ConservativeExecution
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := policy.ExecuteOnQuote(tc.order, tc.quote)
			if err != nil {
				t.Fatalf("ExecuteOnQuote returned error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("fills\n got: %+v\nwant: %+v", got, tc.want)
			}
		})
	}
}

// Scenario: input that cannot describe a real execution is rejected
//
//	Given an order or a quote the domain treats as impossible
//	When execution is attempted
//	Then it reports the class of fault and its precise cause,
//	  instead of inventing a fill.
//
// Order and Quote have exported fields, so their constructors cannot make an
// invalid value unrepresentable. Execution must therefore validate what it is
// handed, not trust that it came from a constructor.
func TestExecuteOnQuoteRejectsImpossibleInput(t *testing.T) {
	good := market.Quote{Instrument: mnq, Time: 1, Bid: 20_000, Ask: 20_001, BidSize: 5, AskSize: 5}

	tests := []struct {
		name      string
		order     market.Order
		quote     market.Quote
		wantClass error
		wantCause error
	}{
		{
			name:      "instrument mismatch",
			order:     mustOrder(t, market.SideBuy, 1),
			quote:     market.Quote{Instrument: market.Instrument{Symbol: "MES"}, Bid: 5, Ask: 6, BidSize: 1, AskSize: 1},
			wantClass: execution.ErrInstrumentMismatch,
			wantCause: execution.ErrInstrumentMismatch,
		},
		{
			name:      "crossed book",
			order:     mustOrder(t, market.SideBuy, 1),
			quote:     market.Quote{Instrument: mnq, Bid: 20_002, Ask: 20_001, BidSize: 1, AskSize: 1},
			wantClass: execution.ErrInvalidQuote,
			wantCause: market.ErrCrossedQuote,
		},
		{
			name:      "negative ask size",
			order:     mustOrder(t, market.SideBuy, 1),
			quote:     market.Quote{Instrument: mnq, Bid: 20_000, Ask: 20_001, BidSize: 1, AskSize: -1},
			wantClass: execution.ErrInvalidQuote,
			wantCause: market.ErrNegativeSize,
		},
		{
			name:      "negative bid size",
			order:     mustOrder(t, market.SideSell, 1),
			quote:     market.Quote{Instrument: mnq, Bid: 20_000, Ask: 20_001, BidSize: -1, AskSize: 1},
			wantClass: execution.ErrInvalidQuote,
			wantCause: market.ErrNegativeSize,
		},
		{
			name:      "zero value order",
			order:     market.Order{},
			quote:     good,
			wantClass: execution.ErrInvalidOrder,
			wantCause: market.ErrEmptyOrderID,
		},
		{
			name:      "unspecified side",
			order:     market.Order{ID: "o-1", Instrument: mnq, Type: market.OrderTypeMarket, Qty: 1},
			quote:     good,
			wantClass: execution.ErrInvalidOrder,
			wantCause: market.ErrInvalidSide,
		},
		{
			name:      "unspecified order type",
			order:     market.Order{ID: "o-1", Instrument: mnq, Side: market.SideBuy, Qty: 1},
			quote:     good,
			wantClass: execution.ErrInvalidOrder,
			wantCause: market.ErrInvalidOrderType,
		},
		{
			name:      "non-positive quantity",
			order:     market.Order{ID: "o-1", Instrument: mnq, Side: market.SideBuy, Type: market.OrderTypeMarket, Qty: 0},
			quote:     good,
			wantClass: execution.ErrInvalidOrder,
			wantCause: market.ErrNonPositiveQty,
		},
		{
			// Regression: an order composed directly, bypassing the
			// constructor, produced a fill that no event log could attribute
			// back to an order.
			name:      "empty order id bypasses the constructor",
			order:     market.Order{ID: "", Instrument: mnq, Side: market.SideBuy, Type: market.OrderTypeMarket, Qty: 1},
			quote:     good,
			wantClass: execution.ErrInvalidOrder,
			wantCause: market.ErrEmptyOrderID,
		},
		{
			// Regression: an order and a quote both carrying the empty
			// instrument compared equal, so the mismatch check passed and a
			// fill was produced for no instrument at all.
			name:      "empty instrument on both sides is not a match",
			order:     market.Order{ID: "o-1", Instrument: market.Instrument{}, Side: market.SideBuy, Type: market.OrderTypeMarket, Qty: 1},
			quote:     market.Quote{Instrument: market.Instrument{}, Time: 1, Bid: 20_000, Ask: 20_001, BidSize: 5, AskSize: 5},
			wantClass: execution.ErrInvalidOrder,
			wantCause: market.ErrEmptySymbol,
		},
		{
			// Regression: a quote with no instrument, against a well formed
			// order, must be rejected as data rather than as a mismatch.
			name:      "quote without an instrument",
			order:     mustOrder(t, market.SideBuy, 1),
			quote:     market.Quote{Instrument: market.Instrument{}, Time: 1, Bid: 20_000, Ask: 20_001, BidSize: 5, AskSize: 5},
			wantClass: execution.ErrInvalidQuote,
			wantCause: market.ErrEmptySymbol,
		},
	}

	var policy execution.ConservativeExecution
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := policy.ExecuteOnQuote(tc.order, tc.quote)
			if !errors.Is(err, tc.wantClass) {
				t.Fatalf("error class: got %v, want %v", err, tc.wantClass)
			}
			if !errors.Is(err, tc.wantCause) {
				t.Fatalf("error cause: got %v, want %v", err, tc.wantCause)
			}
			if got != nil {
				t.Fatalf("rejected input produced fills: %+v", got)
			}
		})
	}
}

func TestNewMarketOrderRejectsInvalidStates(t *testing.T) {
	tests := []struct {
		name string
		id   string
		inst market.Instrument
		side market.Side
		qty  market.Qty
		want error
	}{
		{"empty id", "", mnq, market.SideBuy, 1, market.ErrEmptyOrderID},
		{"empty symbol", "o-1", market.Instrument{}, market.SideBuy, 1, market.ErrEmptySymbol},
		{"unspecified side", "o-1", mnq, market.SideUnspecified, 1, market.ErrInvalidSide},
		{"zero quantity", "o-1", mnq, market.SideBuy, 0, market.ErrNonPositiveQty},
		{"negative quantity", "o-1", mnq, market.SideBuy, -3, market.ErrNonPositiveQty},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := market.NewMarketOrder(tc.id, tc.inst, tc.side, tc.qty)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error: got %v, want %v", err, tc.want)
			}
			if got != (market.Order{}) {
				t.Fatalf("invalid input produced an order: %+v", got)
			}
		})
	}

	o, err := market.NewMarketOrder("o-1", mnq, market.SideSell, 2)
	if err != nil {
		t.Fatalf("valid order rejected: %v", err)
	}
	if o.Type != market.OrderTypeMarket {
		t.Fatalf("order type: got %v, want market", o.Type)
	}
}

// randomQuote and randomOrder generate arbitrary but legal input from an
// explicitly seeded source. No global randomness enters the domain.
func randomQuote(r *rand.Rand) market.Quote {
	bid := market.Ticks(r.Int63n(40_000) - 20_000)
	spread := market.Ticks(r.Int63n(5)) // zero spread means a locked book
	return market.Quote{
		Instrument: mnq,
		Time:       market.LogicalTime(r.Int63n(1_000_000)),
		Bid:        bid,
		Ask:        bid + spread,
		BidSize:    market.Qty(r.Int63n(10)),
		AskSize:    market.Qty(r.Int63n(10)),
	}
}

func randomOrder(r *rand.Rand) market.Order {
	side := market.SideBuy
	if r.Int63n(2) == 1 {
		side = market.SideSell
	}
	return market.Order{
		ID:         "o-1",
		Instrument: mnq,
		Side:       side,
		Type:       market.OrderTypeMarket,
		Qty:        market.Qty(r.Int63n(12) + 1),
	}
}

// Property: execution never lies in favour of the trader. A buy is never
// filled below the ask and a sell is never filled above the bid.
func TestPropertyFillIsNeverBetterThanTheBook(t *testing.T) {
	r := rand.New(rand.NewSource(20240817))
	var policy execution.ConservativeExecution

	for i := 0; i < 5000; i++ {
		q := randomQuote(r)
		o := randomOrder(r)
		fills, err := policy.ExecuteOnQuote(o, q)
		if err != nil {
			t.Fatalf("iteration %d: legal input rejected: %v (order %+v quote %+v)", i, err, o, q)
		}
		for _, f := range fills {
			switch f.Side {
			case market.SideBuy:
				if f.Price < q.Ask {
					t.Fatalf("iteration %d: buy filled at %d, below ask %d", i, f.Price, q.Ask)
				}
			case market.SideSell:
				if f.Price > q.Bid {
					t.Fatalf("iteration %d: sell filled at %d, above bid %d", i, f.Price, q.Bid)
				}
			}
		}
	}
}

// Property: execution never invents liquidity. Filled quantity never exceeds
// the order, never exceeds the size displayed on the taken side, and a fill
// of zero contracts is never emitted.
func TestPropertyFillNeverExceedsOrderOrDisplayedSize(t *testing.T) {
	r := rand.New(rand.NewSource(20240818))
	var policy execution.ConservativeExecution

	for i := 0; i < 5000; i++ {
		q := randomQuote(r)
		o := randomOrder(r)
		available := q.AskSize
		if o.Side == market.SideSell {
			available = q.BidSize
		}

		fills, err := policy.ExecuteOnQuote(o, q)
		if err != nil {
			t.Fatalf("iteration %d: legal input rejected: %v", i, err)
		}

		var total market.Qty
		for _, f := range fills {
			if f.Qty <= 0 {
				t.Fatalf("iteration %d: emitted a fill of %d contracts", i, f.Qty)
			}
			total += f.Qty
		}
		if total > o.Qty {
			t.Fatalf("iteration %d: filled %d of an order for %d", i, total, o.Qty)
		}
		if total > available {
			t.Fatalf("iteration %d: filled %d against displayed size %d", i, total, available)
		}
		if available == 0 && len(fills) != 0 {
			t.Fatalf("iteration %d: filled against an empty side", i)
		}
	}
}

// Property: the same order against the same quote always produces the same
// fills. Determinism is the precondition for every other guarantee.
func TestPropertyExecutionIsDeterministic(t *testing.T) {
	const runs = 20
	var policy execution.ConservativeExecution

	var baseline [][]market.Fill
	for run := 0; run < runs; run++ {
		r := rand.New(rand.NewSource(20240819))
		observed := make([][]market.Fill, 0, 500)
		for i := 0; i < 500; i++ {
			q := randomQuote(r)
			o := randomOrder(r)
			fills, err := policy.ExecuteOnQuote(o, q)
			if err != nil {
				t.Fatalf("run %d iteration %d: legal input rejected: %v", run, i, err)
			}
			observed = append(observed, fills)
		}
		if run == 0 {
			baseline = observed
			continue
		}
		if !reflect.DeepEqual(observed, baseline) {
			t.Fatalf("run %d diverged from the baseline run", run)
		}
	}
}

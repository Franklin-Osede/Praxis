package market_test

import (
	"errors"
	"testing"

	"praxis/internal/market"
)

var mnq = market.Instrument{Symbol: "MNQ", CentsPerTick: 50}

func TestInstrumentValidate(t *testing.T) {
	tests := []struct {
		name string
		i    market.Instrument
		want error
	}{
		{"a complete instrument", mnq, nil},
		{"no symbol", market.Instrument{CentsPerTick: 50}, market.ErrEmptySymbol},
		{"no tick value", market.Instrument{Symbol: "MNQ"}, market.ErrNonPositiveTickValue},
		{"a negative tick value", market.Instrument{Symbol: "MNQ", CentsPerTick: -1}, market.ErrNonPositiveTickValue},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.i.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("error: got %v, want %v", err, tc.want)
			}
		})
	}

	if _, err := market.NewInstrument("MNQ", 0); !errors.Is(err, market.ErrNonPositiveTickValue) {
		t.Fatalf("NewInstrument: got %v, want %v", err, market.ErrNonPositiveTickValue)
	}
	got, err := market.NewInstrument("MNQ", 50)
	if err != nil || got != mnq {
		t.Fatalf("NewInstrument: got (%+v, %v)", got, err)
	}
}

func TestQuoteValidate(t *testing.T) {
	sound := market.Quote{Instrument: mnq, Time: 1, Bid: 20_000, Ask: 20_001, BidSize: 5, AskSize: 5}

	tests := []struct {
		name  string
		quote market.Quote
		want  error
	}{
		{"a two-sided book", sound, nil},
		{"a locked book is tradable", market.Quote{Instrument: mnq, Bid: 20_000, Ask: 20_000, BidSize: 1, AskSize: 1}, nil},
		{"an empty side is a legal observation", market.Quote{Instrument: mnq, Bid: 20_000, Ask: 20_001}, nil},
		{"no instrument", market.Quote{Bid: 1, Ask: 2}, market.ErrEmptySymbol},
		{"a crossed book", market.Quote{Instrument: mnq, Bid: 20_002, Ask: 20_001}, market.ErrCrossedQuote},
		{"a non-positive bid", market.Quote{Instrument: mnq, Bid: 0, Ask: 20_001}, market.ErrNonPositivePrice},
		// Non-positive is checked before crossed: a negative price is the more
		// fundamental fault, and reporting the derived one would send a reader
		// looking for a book that was never the problem.
		{"a negative ask", market.Quote{Instrument: mnq, Bid: 1, Ask: -1}, market.ErrNonPositivePrice},
		{"a negative size", market.Quote{Instrument: mnq, Bid: 1, Ask: 2, BidSize: -1}, market.ErrNegativeSize},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.quote.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("error: got %v, want %v", err, tc.want)
			}
		})
	}

	if !(market.Quote{Bid: 2, Ask: 1}).Crossed() {
		t.Fatal("a bid above an ask is not reported as crossed")
	}
	if (market.Quote{Bid: 1, Ask: 1}).Crossed() {
		t.Fatal("a locked book is reported as crossed")
	}
}

func TestBarValidate(t *testing.T) {
	sound := market.Bar{Instrument: mnq, StartTime: 1, EndTime: 2, Open: 100, High: 110, Low: 90, Close: 105}

	tests := []struct {
		name string
		bar  market.Bar
		want error
	}{
		{"a well formed bar", sound, nil},
		{"volume is optional", sound, nil},
		{"no instrument", market.Bar{StartTime: 1, EndTime: 2, Open: 1, High: 1, Low: 1, Close: 1}, market.ErrEmptySymbol},
		{"an interval that does not advance", market.Bar{Instrument: mnq, StartTime: 5, EndTime: 5, Open: 1, High: 1, Low: 1, Close: 1}, market.ErrEmptyInterval},
		{"an interval that runs backwards", market.Bar{Instrument: mnq, StartTime: 6, EndTime: 5, Open: 1, High: 1, Low: 1, Close: 1}, market.ErrEmptyInterval},
		{"a non-positive low", market.Bar{Instrument: mnq, StartTime: 1, EndTime: 2, Open: 1, High: 1, Low: 0, Close: 1}, market.ErrNonPositivePrice},
		{"an open above the high", market.Bar{Instrument: mnq, StartTime: 1, EndTime: 2, Open: 120, High: 110, Low: 90, Close: 100}, market.ErrOpenOutsideRange},
		{"an open below the low", market.Bar{Instrument: mnq, StartTime: 1, EndTime: 2, Open: 80, High: 110, Low: 90, Close: 100}, market.ErrOpenOutsideRange},
		{"a close outside the range", market.Bar{Instrument: mnq, StartTime: 1, EndTime: 2, Open: 100, High: 110, Low: 90, Close: 120}, market.ErrCloseOutsideRange},
		{"negative volume", market.Bar{Instrument: mnq, StartTime: 1, EndTime: 2, Open: 100, High: 110, Low: 90, Close: 100, Volume: -1}, market.ErrNegativeVolume},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.bar.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("error: got %v, want %v", err, tc.want)
			}
		})
	}
}

// Scenario: an order carries the price its own type needs, and no other
func TestOrderValidate(t *testing.T) {
	base := market.Order{ID: "o-1", Instrument: mnq, Side: market.SideBuy, Qty: 1}

	with := func(f func(*market.Order)) market.Order {
		o := base
		f(&o)
		return o
	}

	tests := []struct {
		name  string
		order market.Order
		want  error
	}{
		{"a market order", with(func(o *market.Order) { o.Type = market.OrderTypeMarket }), nil},
		{"a limit order", with(func(o *market.Order) { o.Type, o.LimitPrice = market.OrderTypeLimit, 20_000 }), nil},
		{"a stop order", with(func(o *market.Order) { o.Type, o.StopPrice = market.OrderTypeStop, 20_000 }), nil},
		{"the zero value", market.Order{}, market.ErrEmptyOrderID},
		{"no identifier", with(func(o *market.Order) { o.ID, o.Type = "", market.OrderTypeMarket }), market.ErrEmptyOrderID},
		{"no instrument", with(func(o *market.Order) { o.Instrument, o.Type = market.Instrument{}, market.OrderTypeMarket }), market.ErrEmptySymbol},
		{"no side", with(func(o *market.Order) { o.Side, o.Type = market.SideUnspecified, market.OrderTypeMarket }), market.ErrInvalidSide},
		{"no type", base, market.ErrInvalidOrderType},
		{"an unknown type", with(func(o *market.Order) { o.Type = 99 }), market.ErrInvalidOrderType},
		{"no quantity", with(func(o *market.Order) { o.Type, o.Qty = market.OrderTypeMarket, 0 }), market.ErrNonPositiveQty},
		{"a limit without its price", with(func(o *market.Order) { o.Type = market.OrderTypeLimit }), market.ErrMissingLimitPrice},
		{"a negative limit", with(func(o *market.Order) { o.Type, o.LimitPrice = market.OrderTypeLimit, -1 }), market.ErrNonPositivePrice},
		{"a stop without its price", with(func(o *market.Order) { o.Type = market.OrderTypeStop }), market.ErrMissingStopPrice},
		{"a negative stop", with(func(o *market.Order) { o.Type, o.StopPrice = market.OrderTypeStop, -1 }), market.ErrNonPositivePrice},
		{"a market order carrying a limit", with(func(o *market.Order) { o.Type, o.LimitPrice = market.OrderTypeMarket, 1 }), market.ErrUnexpectedLimitPrice},
		{"a market order carrying a stop", with(func(o *market.Order) { o.Type, o.StopPrice = market.OrderTypeMarket, 1 }), market.ErrUnexpectedStopPrice},
		{"a limit carrying a stop", with(func(o *market.Order) { o.Type, o.LimitPrice, o.StopPrice = market.OrderTypeLimit, 1, 1 }), market.ErrUnexpectedStopPrice},
		{"a stop carrying a limit", with(func(o *market.Order) { o.Type, o.StopPrice, o.LimitPrice = market.OrderTypeStop, 1, 1 }), market.ErrUnexpectedLimitPrice},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.order.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("error: got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestFillValidate(t *testing.T) {
	sound := market.Fill{OrderID: "o-1", Instrument: mnq, Side: market.SideSell, Price: 20_000, Qty: 2}

	tests := []struct {
		name string
		fill market.Fill
		want error
	}{
		{"a well formed fill", sound, nil},
		{"the zero value", market.Fill{}, market.ErrEmptyOrderID},
		{"no instrument", market.Fill{OrderID: "o-1", Side: market.SideBuy, Price: 1, Qty: 1}, market.ErrEmptySymbol},
		{"no side", market.Fill{OrderID: "o-1", Instrument: mnq, Price: 1, Qty: 1}, market.ErrInvalidSide},
		{"a non-positive price", market.Fill{OrderID: "o-1", Instrument: mnq, Side: market.SideBuy, Qty: 1}, market.ErrNonPositivePrice},
		{"no quantity", market.Fill{OrderID: "o-1", Instrument: mnq, Side: market.SideBuy, Price: 1}, market.ErrNonPositiveQty},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.fill.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("error: got %v, want %v", err, tc.want)
			}
		})
	}

	// Direction lives in Side. A fill's own quantity is always positive, and
	// SignedQty is the only place that turns it into a position effect.
	buy := market.Fill{Side: market.SideBuy, Qty: 3}
	sell := market.Fill{Side: market.SideSell, Qty: 3}
	if buy.SignedQty() != 3 || sell.SignedQty() != -3 {
		t.Fatalf("signed quantities: buy %d, sell %d", buy.SignedQty(), sell.SignedQty())
	}
}

// The zero value of every enumeration is invalid, so an uninitialised struct
// cannot pass for a meaningful one.
func TestZeroValuesAreNeverMeaningful(t *testing.T) {
	if market.SideUnspecified.Valid() {
		t.Fatal("the zero side is valid")
	}
	if market.SideUnspecified.String() != "unspecified" || market.OrderTypeUnspecified.String() != "unspecified" {
		t.Fatal("an unspecified value does not name itself")
	}
	for _, s := range []market.Side{market.SideBuy, market.SideSell} {
		if !s.Valid() || s.String() == "unspecified" {
			t.Fatalf("side %d is not usable", s)
		}
	}
	for _, ot := range []market.OrderType{market.OrderTypeMarket, market.OrderTypeLimit, market.OrderTypeStop} {
		if ot.String() == "unspecified" {
			t.Fatalf("order type %d does not name itself", ot)
		}
	}
}

func TestConstructorsRejectWhatValidateWouldRefuse(t *testing.T) {
	if _, err := market.NewMarketOrder("", mnq, market.SideBuy, 1); !errors.Is(err, market.ErrEmptyOrderID) {
		t.Fatalf("NewMarketOrder: %v", err)
	}
	if _, err := market.NewLimitOrder("o-1", mnq, market.SideBuy, 1, 0); !errors.Is(err, market.ErrMissingLimitPrice) {
		t.Fatalf("NewLimitOrder: %v", err)
	}
	if _, err := market.NewStopOrder("o-1", mnq, market.SideBuy, 1, 0); !errors.Is(err, market.ErrMissingStopPrice) {
		t.Fatalf("NewStopOrder: %v", err)
	}

	for _, built := range []func() (market.Order, error){
		func() (market.Order, error) { return market.NewMarketOrder("o-1", mnq, market.SideBuy, 1) },
		func() (market.Order, error) { return market.NewLimitOrder("o-1", mnq, market.SideBuy, 1, 20_000) },
		func() (market.Order, error) { return market.NewStopOrder("o-1", mnq, market.SideBuy, 1, 20_000) },
	} {
		o, err := built()
		if err != nil {
			t.Fatalf("a valid order was refused: %v", err)
		}
		if err := o.Validate(); err != nil {
			t.Fatalf("a constructed order does not validate: %v", err)
		}
	}
}

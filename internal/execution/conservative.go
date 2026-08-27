// Package execution turns orders and market observations into fills. It
// decides nothing about accounts, risk or prop-firm rules: it produces facts.
package execution

import (
	"errors"

	"praxis/internal/market"
)

// Errors reported when input cannot describe a real execution. They are
// programming or data faults, not failed executions: an order that simply
// finds no liquidity returns no fills and no error.
var (
	ErrInstrumentMismatch = errors.New("execution: order and quote are for different instruments")
	ErrCrossedQuote       = errors.New("execution: quote is crossed")
	ErrInvalidQuote       = errors.New("execution: quote has a negative displayed size")
	ErrInvalidOrder       = errors.New("execution: order cannot be executed as written")
)

// ConservativeExecution never grants the trader a price or a quantity the
// observed book does not justify.
type ConservativeExecution struct{}

// ExecuteOnQuote executes an order against a single top-of-book observation.
//
// A market order crosses the spread: a buy pays the ask, a sell hits the bid,
// and neither ever receives a better price. It takes at most the quantity the
// taken side displays, because assuming depth that was never observed is the
// most common way a simulator flatters the trader. The unfilled remainder is
// the caller's to carry; this policy holds no state.
//
// Finding no liquidity is not an error: it returns no fills and no error.
func (ConservativeExecution) ExecuteOnQuote(o market.Order, q market.Quote) ([]market.Fill, error) {
	if o.Type != market.OrderTypeMarket || !o.Side.Valid() || o.Qty <= 0 {
		return nil, ErrInvalidOrder
	}
	if o.Instrument != q.Instrument {
		return nil, ErrInstrumentMismatch
	}
	if q.Crossed() {
		return nil, ErrCrossedQuote
	}
	if q.BidSize < 0 || q.AskSize < 0 {
		return nil, ErrInvalidQuote
	}

	price, available := q.Bid, q.BidSize
	if o.Side == market.SideBuy {
		price, available = q.Ask, q.AskSize
	}

	filled := o.Qty
	if available < filled {
		filled = available
	}
	if filled <= 0 {
		return nil, nil
	}

	return []market.Fill{{
		OrderID:    o.ID,
		Instrument: o.Instrument,
		Time:       q.Time,
		Side:       o.Side,
		Price:      price,
		Qty:        filled,
	}}, nil
}

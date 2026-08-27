// Package execution turns orders and market observations into fills. It
// decides nothing about accounts, risk or prop-firm rules: it produces facts.
package execution

import (
	"errors"
	"fmt"

	"praxis/internal/market"
)

// Errors reported when input cannot describe a real execution. They are
// programming or data faults, not failed executions: an order that simply
// finds no liquidity returns no fills and no error.
var (
	ErrInstrumentMismatch = errors.New("execution: order and quote are for different instruments")
	ErrInvalidOrder       = errors.New("execution: order is not a valid domain value")
	ErrInvalidQuote       = errors.New("execution: quote is not a valid domain value")
)

// ConservativeExecution never grants the trader a price or a quantity the
// observed book does not justify.
type ConservativeExecution struct{}

// ExecuteOnQuote executes an order against a single top-of-book observation.
//
// A market order crosses the spread: a buy pays the ask, a sell hits the bid,
// and neither ever receives a better price.
//
// A limit order is executable when the touch on the taken side has reached its
// limit, and then fills at its own limit rather than at the touch. A real
// resting limit order can be filled better than its limit; granting that in a
// simulator teaches a habit the market will not honour. See "Limit orders fill
// at their limit price" in docs/PRAXIS_SPEC.md section 4 before changing this.
//
// Either kind takes at most the quantity the taken side displays, because
// assuming depth that was never observed is the most common way a simulator
// flatters the trader. The unfilled remainder is the caller's to carry; this
// policy holds no state.
//
// Neither finding no liquidity nor being unexecutable is an error: both return
// no fills and no error.
func (ConservativeExecution) ExecuteOnQuote(o market.Order, q market.Quote) ([]market.Fill, error) {
	if err := o.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidOrder, err)
	}
	if err := q.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidQuote, err)
	}
	if o.Instrument != q.Instrument {
		return nil, ErrInstrumentMismatch
	}

	touch, available := q.Bid, q.BidSize
	if o.Side == market.SideBuy {
		touch, available = q.Ask, q.AskSize
	}

	price := touch
	if o.Type == market.OrderTypeLimit {
		if !reachedLimit(o.Side, touch, o.LimitPrice) {
			return nil, nil
		}
		price = o.LimitPrice
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

// reachedLimit reports whether the touch on the taken side has come to the
// limit price: at or below it for a buy, at or above it for a sell.
func reachedLimit(side market.Side, touch, limit market.Ticks) bool {
	if side == market.SideBuy {
		return touch <= limit
	}
	return touch >= limit
}

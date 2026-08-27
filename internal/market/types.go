// Package market holds the vocabulary of observed market data, and the orders
// and fills expressed in it.
//
// Rules enforced here (see AGENTS.md):
//   - no binary floating-point for prices, quantities or money;
//   - no time.Now(): time is always data, carried by the observation;
//   - no infrastructure imports.
package market

import "errors"

// Ticks is a price expressed in whole instrument ticks. Converting ticks to
// money requires the instrument's immutable monetary tick value, which does
// not exist yet because nothing in this slice computes money.
type Ticks int64

// Qty is a contract count. Order and fill quantities are strictly positive;
// direction is carried by Side, never by the sign of Qty.
type Qty int64

// LogicalTime is nanoseconds since the Unix epoch in UTC. It is the domain's
// only notion of time.
type LogicalTime int64

// Side is the direction of an order or fill.
type Side uint8

const (
	// SideUnspecified is the zero value and is never valid input.
	SideUnspecified Side = iota
	SideBuy
	SideSell
)

func (s Side) String() string {
	switch s {
	case SideBuy:
		return "buy"
	case SideSell:
		return "sell"
	default:
		return "unspecified"
	}
}

// Valid reports whether s names a real direction.
func (s Side) Valid() bool { return s == SideBuy || s == SideSell }

// Instrument identifies a tradable contract. It carries only what a consumer
// in this slice needs; monetary specification arrives with the first code
// that converts ticks to money.
type Instrument struct {
	Symbol string
}

// Quote is a two-sided top-of-book observation at a point in logical time.
type Quote struct {
	Instrument Instrument
	Time       LogicalTime
	Bid        Ticks
	Ask        Ticks
	BidSize    Qty
	AskSize    Qty
}

// Crossed reports whether the book is crossed, meaning the bid is strictly
// above the ask. A crossed book is invalid input rather than a tradable
// state. A locked book (bid equal to ask) is legal and tradable.
func (q Quote) Crossed() bool { return q.Bid > q.Ask }

// OrderType distinguishes execution semantics. Only market orders exist in
// this slice; limit and stop orders arrive with their own tests.
type OrderType uint8

const (
	// OrderTypeUnspecified is the zero value and is never valid input.
	OrderTypeUnspecified OrderType = iota
	OrderTypeMarket
)

func (t OrderType) String() string {
	if t == OrderTypeMarket {
		return "market"
	}
	return "unspecified"
}

// Order is a trader instruction. Construct it through a constructor so that
// an invalid order cannot be represented by accident.
type Order struct {
	ID         string
	Instrument Instrument
	Side       Side
	Type       OrderType
	Qty        Qty
}

// Errors returned when an order or an instrument cannot be constructed.
var (
	ErrEmptyOrderID   = errors.New("market: order id is empty")
	ErrEmptySymbol    = errors.New("market: instrument symbol is empty")
	ErrInvalidSide    = errors.New("market: side is unspecified")
	ErrNonPositiveQty = errors.New("market: order quantity is not positive")
)

// NewMarketOrder builds a market order, rejecting states the domain treats as
// impossible rather than as failed executions.
func NewMarketOrder(id string, i Instrument, side Side, qty Qty) (Order, error) {
	if id == "" {
		return Order{}, ErrEmptyOrderID
	}
	if i.Symbol == "" {
		return Order{}, ErrEmptySymbol
	}
	if !side.Valid() {
		return Order{}, ErrInvalidSide
	}
	if qty <= 0 {
		return Order{}, ErrNonPositiveQty
	}
	return Order{ID: id, Instrument: i, Side: side, Type: OrderTypeMarket, Qty: qty}, nil
}

// Fill is an executed quantity at an executed price. It records the logical
// time of the observation that produced it, never a wall clock reading.
type Fill struct {
	OrderID    string
	Instrument Instrument
	Time       LogicalTime
	Side       Side
	Price      Ticks
	Qty        Qty
}

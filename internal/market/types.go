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

// Validate reports why the quote is not a valid domain value, or nil. A
// quote is composable field by field, so its consumers check it for the same
// reason Order.Validate exists. Being untradable is not invalid: an empty
// side is a legal observation.
func (q Quote) Validate() error {
	if q.Instrument.Symbol == "" {
		return ErrEmptySymbol
	}
	if q.Crossed() {
		return ErrCrossedQuote
	}
	if q.BidSize < 0 || q.AskSize < 0 {
		return ErrNegativeSize
	}
	return nil
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

// Errors describing why an order or a quote is not a valid domain value.
var (
	ErrEmptyOrderID     = errors.New("market: order id is empty")
	ErrEmptySymbol      = errors.New("market: instrument symbol is empty")
	ErrInvalidSide      = errors.New("market: side is unspecified")
	ErrInvalidOrderType = errors.New("market: order type is unspecified")
	ErrNonPositiveQty   = errors.New("market: order quantity is not positive")
	ErrCrossedQuote     = errors.New("market: quote is crossed")
	ErrNegativeSize     = errors.New("market: quote has a negative displayed size")
)

// Validate reports why the order is not a valid domain value, or nil.
//
// Order has exported fields, so a constructor cannot make an invalid order
// unrepresentable: any caller can compose one directly. Validity therefore
// lives on the value itself and every consumer checks it, so that the
// constructor and the consumers cannot drift apart.
func (o Order) Validate() error {
	if o.ID == "" {
		return ErrEmptyOrderID
	}
	if o.Instrument.Symbol == "" {
		return ErrEmptySymbol
	}
	if !o.Side.Valid() {
		return ErrInvalidSide
	}
	if o.Type != OrderTypeMarket {
		return ErrInvalidOrderType
	}
	if o.Qty <= 0 {
		return ErrNonPositiveQty
	}
	return nil
}

// NewMarketOrder builds a market order, rejecting states the domain treats as
// impossible rather than as failed executions.
func NewMarketOrder(id string, i Instrument, side Side, qty Qty) (Order, error) {
	o := Order{ID: id, Instrument: i, Side: side, Type: OrderTypeMarket, Qty: qty}
	if err := o.Validate(); err != nil {
		return Order{}, err
	}
	return o, nil
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

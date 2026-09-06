// Package market holds the vocabulary of observed market data, and the orders
// and fills expressed in it.
//
// Rules enforced here (see AGENTS.md):
//   - no binary floating-point for prices, quantities or money;
//   - no time.Now(): time is always data, carried by the observation;
//   - no infrastructure imports.
package market

import (
	"errors"
	"fmt"
	"math"
)

// Ticks is a price expressed in whole instrument ticks. Converting ticks to
// money requires the instrument's immutable monetary tick value, which does
// not exist yet because nothing in this slice computes money.
type Ticks int64

// Cents is a monetary amount in whole cents. It is the only money unit in the
// domain; an instrument whose tick value cannot be expressed exactly in it is
// rejected rather than approximated.
type Cents int64

// Qty is a contract count. Order and fill quantities are strictly positive;
// direction is carried by Side, never by the sign of Qty.
type Qty int64

// LogicalTime is nanoseconds since the Unix epoch in UTC. It is the domain's
// only notion of time, and it is the market's: every execution, valuation and
// rule reads it and nothing else.
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

// Instrument identifies a tradable contract and carries its immutable
// monetary specification.
type Instrument struct {
	Symbol string

	// CentsPerTick is the money value of one tick of price movement in one
	// contract. It belongs to the instrument, never to a global constant, and
	// an instrument whose real tick value is not a whole number of cents is
	// not representable and must be rejected rather than rounded.
	CentsPerTick Cents
}

// NewInstrument builds an instrument, rejecting one whose monetary tick value
// cannot be represented exactly.
func NewInstrument(symbol string, centsPerTick Cents) (Instrument, error) {
	i := Instrument{Symbol: symbol, CentsPerTick: centsPerTick}
	if err := i.Validate(); err != nil {
		return Instrument{}, err
	}
	return i, nil
}

// Validate reports why the instrument is not a valid domain value, or nil.
func (i Instrument) Validate() error {
	if i.Symbol == "" {
		return ErrEmptySymbol
	}
	// A symbol is written into the first event of every journal, so one the
	// record cannot hold is not a bad instrument, it is a journal that cannot
	// begin. It arrives from a file's header like every other identifier here
	// arrives from outside.
	if err := ValidIdentifier(i.Symbol); err != nil {
		return err
	}
	if i.CentsPerTick <= 0 {
		return ErrNonPositiveTickValue
	}
	return nil
}

// Money converts a price in ticks and a contract count into cents. Both
// arguments are taken as given: direction and sign are the caller's business.
//
// It reports an error rather than wrapping silently. Go's integer overflow is
// silent, so exact accounting is only exact until it happens.
func (i Instrument) Money(price Ticks, qty Qty) (Cents, error) {
	perContract, err := MulCents(Cents(price), i.CentsPerTick)
	if err != nil {
		return 0, err
	}
	return MulCents(perContract, Cents(qty))
}

// ErrOverflow reports arithmetic that cannot be represented.
var ErrOverflow = errors.New("market: integer overflow")

// MulCents, AddCents and SubCents are checked arithmetic. Every monetary
// operation in the domain goes through them, because Go's integer overflow is
// silent and exact accounting is only exact until it happens.
func MulCents(a, b Cents) (Cents, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	r := a * b
	if r/a != b || (a == -1 && b == math.MinInt64) || (b == -1 && a == math.MinInt64) {
		return 0, fmt.Errorf("%w: %d * %d", ErrOverflow, a, b)
	}
	return r, nil
}

func AddCents(a, b Cents) (Cents, error) {
	r := a + b
	if (r > a) != (b > 0) {
		return 0, fmt.Errorf("%w: %d + %d", ErrOverflow, a, b)
	}
	return r, nil
}

func SubCents(a, b Cents) (Cents, error) {
	r := a - b
	if (r < a) != (b > 0) {
		return 0, fmt.Errorf("%w: %d - %d", ErrOverflow, a, b)
	}
	return r, nil
}

// AddQty is checked addition of contract counts.
func AddQty(a, b Qty) (Qty, error) {
	r := a + b
	if (r > a) != (b > 0) {
		return 0, fmt.Errorf("%w: %d + %d contracts", ErrOverflow, a, b)
	}
	return r, nil
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
	if err := q.Instrument.Validate(); err != nil {
		return err
	}
	if q.Bid <= 0 || q.Ask <= 0 {
		return ErrNonPositivePrice
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

// Bar is a completed interval of market observations, supplied by an adapter.
// The kernel never builds one: see ADR-010. The interval convention is
// [StartTime, EndTime).
type Bar struct {
	Instrument Instrument
	StartTime  LogicalTime
	EndTime    LogicalTime
	Open       Ticks
	High       Ticks
	Low        Ticks
	Close      Ticks

	// Volume is optional and meaningful only when the source configuration
	// defines what it counts. Zero means unstated, never "no trades".
	Volume Qty

	// Sequence orders bars that share an EndTime. Bars are emitted in
	// ascending (EndTime, Sequence) order.
	Sequence uint64
}

// Validate reports why the bar is not a valid domain value, or nil. A bar is
// composable field by field, so its consumers check it for the same reason
// Order.Validate exists.
func (b Bar) Validate() error {
	if err := b.Instrument.Validate(); err != nil {
		return err
	}
	if b.StartTime >= b.EndTime {
		return ErrEmptyInterval
	}
	if b.Low <= 0 {
		return ErrNonPositivePrice
	}
	if b.Open < b.Low || b.Open > b.High {
		return ErrOpenOutsideRange
	}
	if b.Close < b.Low || b.Close > b.High {
		return ErrCloseOutsideRange
	}
	if b.Volume < 0 {
		return ErrNegativeVolume
	}
	return nil
}

// OrderType distinguishes execution semantics.
type OrderType uint8

const (
	// OrderTypeUnspecified is the zero value and is never valid input.
	OrderTypeUnspecified OrderType = iota
	OrderTypeMarket
	OrderTypeLimit
	OrderTypeStop
)

func (t OrderType) String() string {
	switch t {
	case OrderTypeMarket:
		return "market"
	case OrderTypeLimit:
		return "limit"
	case OrderTypeStop:
		return "stop"
	default:
		return "unspecified"
	}
}

// Order is a trader instruction. Construct it through a constructor so that
// an invalid order cannot be represented by accident.
type Order struct {
	ID         string
	Instrument Instrument
	Side       Side
	Type       OrderType
	Qty        Qty

	// LimitPrice is the worst price a limit order accepts. It is meaningful
	// only for OrderTypeLimit and must be left at zero otherwise, so that a
	// price set on an order whose type was never changed cannot be ignored
	// in silence.
	LimitPrice Ticks

	// StopPrice is the level at which a stop order becomes a market order. It
	// is meaningful only for OrderTypeStop and, like LimitPrice, must be left
	// at zero otherwise. It is not a price the order is promised.
	StopPrice Ticks
}

// Errors describing why an order or a quote is not a valid domain value.
var (
	ErrEmptyOrderID         = errors.New("market: order id is empty")
	ErrIdentifierCharacter  = errors.New("market: identifier uses a character that cannot be written down")
	ErrReservedIdentifier   = errors.New("market: identifier is the one the record uses to mean absent")
	ErrEmptySymbol          = errors.New("market: instrument symbol is empty")
	ErrInvalidSide          = errors.New("market: side is unspecified")
	ErrInvalidOrderType     = errors.New("market: order type is unknown")
	ErrMissingLimitPrice    = errors.New("market: limit order has no limit price")
	ErrUnexpectedLimitPrice = errors.New("market: non-limit order carries a limit price")
	ErrMissingStopPrice     = errors.New("market: stop order has no stop price")
	ErrUnexpectedStopPrice  = errors.New("market: non-stop order carries a stop price")
	ErrNonPositiveQty       = errors.New("market: order quantity is not positive")
	ErrCrossedQuote         = errors.New("market: quote is crossed")
	ErrNegativeSize         = errors.New("market: quote has a negative displayed size")
	ErrEmptyInterval        = errors.New("market: bar interval does not start before it ends")
	ErrOpenOutsideRange     = errors.New("market: bar open is outside its low-high range")
	ErrCloseOutsideRange    = errors.New("market: bar close is outside its low-high range")
	ErrNegativeVolume       = errors.New("market: bar volume is negative")
	ErrNonPositiveTickValue = errors.New("market: instrument tick value is not positive")
	ErrNonPositivePrice     = errors.New("market: price is not positive")
)

// Validate reports why the order is not a valid domain value, or nil.
//
// Order has exported fields, so a constructor cannot make an invalid order
// unrepresentable: any caller can compose one directly. Validity therefore
// lives on the value itself and every consumer checks it, so that the
// constructor and the consumers cannot drift apart.
// ValidIdentifier refuses a name the record cannot hold.
//
// The allowed set — letters, digits, and . _ : - — is **the file format's**,
// adopted here rather than derived from anything about markets. It is the one
// place the domain takes a rule from the shape of its own record, and it does
// so because the alternative is worse: a name the kernel
// accepts and the journal cannot write is a valid command that poisons the
// session at commit time, three batches in, with the next perfectly good order
// refused after it. A system whose entire purpose is the record cannot let a
// decision exist that the record has no way to contain.
//
// A protective leg is named praxis:<sequence>:stop, which is why the colon is
// in the set. The restriction is acceptable for the instruments in use and is
// not a claim about what a tradable instrument may be called: a symbol set that
// needs other characters is a reason to teach the format an encoding, not a
// reason to call the instrument invalid. See section 11 of PRAXIS_SPEC.md. A lone "-" is refused because that is what the record writes for
// a name that is absent: a thing actually called "-" would be indistinguishable
// from nothing, and the reader would blame whichever field went missing rather
// than the name that caused it.
func ValidIdentifier(s string) error {
	if s == "" {
		return ErrEmptyOrderID
	}
	if s == "-" {
		return ErrReservedIdentifier
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == ':' || r == '-':
		default:
			return fmt.Errorf("%w: %q in %q", ErrIdentifierCharacter, r, s)
		}
	}
	return nil
}

func (o Order) Validate() error {
	if err := ValidIdentifier(o.ID); err != nil {
		return err
	}
	if err := o.Instrument.Validate(); err != nil {
		return err
	}
	if !o.Side.Valid() {
		return ErrInvalidSide
	}
	// A zero price cannot be told apart from an unset field, and no supported
	// instrument trades at zero ticks, so zero means unset. Each type requires
	// its own price and must carry no other, so a price left on an order whose
	// type was changed cannot be ignored in silence.
	switch o.Type {
	case OrderTypeMarket:
	case OrderTypeLimit:
		if o.LimitPrice == 0 {
			return ErrMissingLimitPrice
		}
		if o.LimitPrice < 0 {
			return ErrNonPositivePrice
		}
	case OrderTypeStop:
		if o.StopPrice == 0 {
			return ErrMissingStopPrice
		}
		if o.StopPrice < 0 {
			return ErrNonPositivePrice
		}
	default:
		return ErrInvalidOrderType
	}
	if o.Type != OrderTypeLimit && o.LimitPrice != 0 {
		return ErrUnexpectedLimitPrice
	}
	if o.Type != OrderTypeStop && o.StopPrice != 0 {
		return ErrUnexpectedStopPrice
	}
	if o.Qty <= 0 {
		return ErrNonPositiveQty
	}
	return nil
}

// NewLimitOrder builds a limit order. The limit is the worst price the order
// accepts, not a price it is guaranteed to better.
func NewLimitOrder(id string, i Instrument, side Side, qty Qty, limit Ticks) (Order, error) {
	o := Order{ID: id, Instrument: i, Side: side, Type: OrderTypeLimit, Qty: qty, LimitPrice: limit}
	if err := o.Validate(); err != nil {
		return Order{}, err
	}
	return o, nil
}

// NewStopOrder builds a stop order. The stop is the level that triggers a
// market order, not a price the order is promised: once triggered it takes
// whatever the book shows, which on a gap is worse than the level.
func NewStopOrder(id string, i Instrument, side Side, qty Qty, stop Ticks) (Order, error) {
	o := Order{ID: id, Instrument: i, Side: side, Type: OrderTypeStop, Qty: qty, StopPrice: stop}
	if err := o.Validate(); err != nil {
		return Order{}, err
	}
	return o, nil
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

// Validate reports why the fill is not a valid domain value, or nil.
func (f Fill) Validate() error {
	if f.OrderID == "" {
		return ErrEmptyOrderID
	}
	if err := f.Instrument.Validate(); err != nil {
		return err
	}
	if !f.Side.Valid() {
		return ErrInvalidSide
	}
	if f.Price <= 0 {
		return ErrNonPositivePrice
	}
	if f.Qty <= 0 {
		return ErrNonPositiveQty
	}
	return nil
}

// SignedQty is the fill's effect on a net position: positive for a buy,
// negative for a sell.
func (f Fill) SignedQty() Qty {
	if f.Side == SideSell {
		return -f.Qty
	}
	return f.Qty
}

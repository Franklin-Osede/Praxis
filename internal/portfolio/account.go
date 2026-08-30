package portfolio

import (
	"errors"
	"fmt"

	"praxis/internal/market"
)

// PositionEventKind names what applying a fill did to a position. These are
// the facts Phase 3 will persist; the behavioural context around them belongs
// to that slice, not to this one.
type PositionEventKind uint8

const (
	PositionOpened PositionEventKind = iota + 1
	PositionIncreased
	PositionReduced
	PositionClosed
)

func (k PositionEventKind) String() string {
	switch k {
	case PositionOpened:
		return "opened"
	case PositionIncreased:
		return "increased"
	case PositionReduced:
		return "reduced"
	case PositionClosed:
		return "closed"
	default:
		return "unspecified"
	}
}

// PositionEvent is one leg of what a fill did. A fill that flips a position
// produces two: the close and the new open.
type PositionEvent struct {
	Kind        PositionEventKind
	Instrument  market.Instrument
	Side        market.Side
	Qty         market.Qty
	Price       market.Ticks
	RealisedCts market.Cents
	FeeCts      market.Cents
}

// Mark is a price at which an open position is valued.
type Mark struct {
	Instrument market.Instrument
	Price      market.Ticks
}

// Errors reported by an account.
var (
	ErrNonPositiveBalance = errors.New("portfolio: starting balance is not positive")
	ErrNegativeCommission = errors.New("portfolio: commission per contract is negative")
	ErrInvalidFill        = errors.New("portfolio: fill is not a valid domain value")
	ErrMissingMark        = errors.New("portfolio: no mark for an open position")

	// ErrInstrumentSpecMismatch guards the identity of an instrument. A
	// symbol is not an instrument: two values carrying the same symbol and a
	// different monetary specification would accumulate incompatible cost
	// bases in one position and manufacture P&L with no price movement.
	ErrInstrumentSpecMismatch = errors.New("portfolio: symbol already known with a different specification")
)

// Account is a trading account: a starting balance, the positions it holds,
// and the realised money and fees accumulated by the fills applied to it.
type Account struct {
	startingCts   market.Cents
	commissionCts market.Cents
	realisedCts   market.Cents
	feesCts       market.Cents

	// positions is ordered by instrument symbol. It is a slice and not a map
	// because iteration order must never affect a result.
	positions []Position
}

// NewAccount builds an account with a starting balance and a flat commission
// charged per contract on every filled leg.
func NewAccount(startingCts, commissionPerContractCts market.Cents) (*Account, error) {
	if startingCts <= 0 {
		return nil, ErrNonPositiveBalance
	}
	if commissionPerContractCts < 0 {
		return nil, ErrNegativeCommission
	}
	return &Account{startingCts: startingCts, commissionCts: commissionPerContractCts}, nil
}

func (a *Account) StartingBalanceCts() market.Cents { return a.startingCts }
func (a *Account) RealisedCts() market.Cents        { return a.realisedCts }
func (a *Account) FeesCts() market.Cents            { return a.feesCts }

// Positions returns the account's positions in instrument order, including
// those that have closed to flat.
func (a *Account) Positions() []Position {
	out := make([]Position, len(a.positions))
	copy(out, a.positions)
	return out
}

// Position returns the account's position in one instrument.
func (a *Account) Position(i market.Instrument) (Position, bool) {
	if n, err := a.indexOf(i); err == nil && n >= 0 {
		return a.positions[n], true
	}
	return Position{}, false
}

// indexOf returns the index of the instrument's position, or -1 if the account
// has never seen the symbol. It reports an error when the symbol is known
// under a different monetary specification: within one account a symbol names
// exactly one instrument. Positions are kept sorted by symbol, so this scan is
// deterministic.
func (a *Account) indexOf(i market.Instrument) (int, error) {
	for n, p := range a.positions {
		if p.Instrument.Symbol != i.Symbol {
			continue
		}
		if p.Instrument != i {
			return -1, fmt.Errorf("%w: %s known as %d cents per tick, given %d",
				ErrInstrumentSpecMismatch, i.Symbol, p.Instrument.CentsPerTick, i.CentsPerTick)
		}
		return n, nil
	}
	return -1, nil
}

// openPosition inserts a flat position for the instrument, keeping the slice
// ordered by symbol, and returns its index.
func (a *Account) openPosition(i market.Instrument) int {
	at := len(a.positions)
	for n, p := range a.positions {
		if p.Instrument.Symbol > i.Symbol {
			at = n
			break
		}
	}
	a.positions = append(a.positions, Position{})
	copy(a.positions[at+1:], a.positions[at:])
	a.positions[at] = Position{Instrument: i}
	return at
}

// ApplyFill applies a fill and reports what it did.
//
// A fill that reduces a position realises P&L in proportion to the cost
// removed, using weighted average cost rather than lot tracking. A fill that
// reduces past flat is a close followed by a new open: both legs are reported
// and both are charged commission, and the new position starts from a cost
// basis of its own fill price with nothing carried over.
//
// Every figure is computed with checked arithmetic into local state, and the
// account is only written once all of it has succeeded. A rejected fill
// therefore leaves position, realised P&L and fees exactly as they were.
func (a *Account) ApplyFill(f market.Fill) ([]PositionEvent, error) {
	if err := f.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidFill, err)
	}

	n, err := a.indexOf(f.Instrument)
	if err != nil {
		return nil, err
	}

	pos := Position{Instrument: f.Instrument}
	if n >= 0 {
		pos = a.positions[n]
	}

	signed := f.SignedQty()
	var (
		events      []PositionEvent
		realisedCts market.Cents
		feesCts     market.Cents
	)

	charge := func(e PositionEvent) error {
		fee, err := market.MulCents(market.Cents(e.Qty), a.commissionCts)
		if err != nil {
			return err
		}
		total, err := market.AddCents(feesCts, fee)
		if err != nil {
			return err
		}
		feesCts = total
		e.FeeCts = fee
		events = append(events, e)
		return nil
	}

	// Reduce first: an opposite-sided fill closes what it can before any
	// remainder opens a new position.
	if opposed(pos.NetQty, signed) {
		closeQty := min(abs(signed), abs(pos.NetQty))
		direction := market.Qty(sign(pos.NetQty))

		removedCts, err := allocateCost(pos.CostBasisCts, closeQty, abs(pos.NetQty))
		if err != nil {
			return nil, err
		}
		proceedsCts, err := f.Instrument.Money(f.Price, direction*closeQty)
		if err != nil {
			return nil, err
		}
		legRealisedCts, err := market.SubCents(proceedsCts, removedCts)
		if err != nil {
			return nil, err
		}
		if realisedCts, err = market.AddCents(realisedCts, legRealisedCts); err != nil {
			return nil, err
		}
		if pos.CostBasisCts, err = market.SubCents(pos.CostBasisCts, removedCts); err != nil {
			return nil, err
		}
		if pos.NetQty, err = market.AddQty(pos.NetQty, -direction*closeQty); err != nil {
			return nil, err
		}

		kind := PositionReduced
		if pos.NetQty == 0 {
			kind = PositionClosed
		}
		if err := charge(PositionEvent{
			Kind: kind, Instrument: f.Instrument, Side: f.Side,
			Qty: closeQty, Price: f.Price, RealisedCts: legRealisedCts,
		}); err != nil {
			return nil, err
		}

		signed = remainderAfterClose(signed, closeQty)
	}

	if signed != 0 {
		kind := PositionIncreased
		if pos.NetQty == 0 {
			kind = PositionOpened
		}
		openedCts, err := f.Instrument.Money(f.Price, signed)
		if err != nil {
			return nil, err
		}
		if pos.CostBasisCts, err = market.AddCents(pos.CostBasisCts, openedCts); err != nil {
			return nil, err
		}
		if pos.NetQty, err = market.AddQty(pos.NetQty, signed); err != nil {
			return nil, err
		}
		if err := charge(PositionEvent{
			Kind: kind, Instrument: f.Instrument, Side: f.Side,
			Qty: abs(signed), Price: f.Price,
		}); err != nil {
			return nil, err
		}
	}

	// The position must be coherent before anything is written: a flat
	// position holds no cost, and an open one holds a cost whose sign matches
	// its direction.
	if err := pos.Validate(); err != nil {
		return nil, err
	}

	newRealisedCts, err := market.AddCents(a.realisedCts, realisedCts)
	if err != nil {
		return nil, err
	}
	newFeesCts, err := market.AddCents(a.feesCts, feesCts)
	if err != nil {
		return nil, err
	}

	// Commit.
	if n < 0 {
		n = a.openPosition(f.Instrument)
	}
	a.positions[n] = pos
	a.realisedCts = newRealisedCts
	a.feesCts = newFeesCts
	return events, nil
}

// UnrealisedCts values every open position at the given marks.
func (a *Account) UnrealisedCts(marks []Mark) (market.Cents, error) {
	var total market.Cents
	for _, p := range a.positions {
		if p.IsFlat() {
			continue
		}
		mark, err := markFor(marks, p.Instrument)
		if err != nil {
			return 0, err
		}
		value, err := p.UnrealisedCts(mark)
		if err != nil {
			return 0, err
		}
		if total, err = market.AddCents(total, value); err != nil {
			return 0, err
		}
	}
	return total, nil
}

// markFor looks a mark up by symbol. Marks are an ordered slice and not a map
// so that a missing or duplicated mark behaves identically on every run.
func markFor(marks []Mark, i market.Instrument) (market.Ticks, error) {
	for _, m := range marks {
		if m.Instrument.Symbol != i.Symbol {
			continue
		}
		if m.Instrument != i {
			return 0, fmt.Errorf("%w: %s marked as %d cents per tick, held as %d",
				ErrInstrumentSpecMismatch, i.Symbol, m.Instrument.CentsPerTick, i.CentsPerTick)
		}
		if m.Price <= 0 {
			return 0, fmt.Errorf("%w: mark for %s", market.ErrNonPositivePrice, i.Symbol)
		}
		return m.Price, nil
	}
	return 0, fmt.Errorf("%w: %s", ErrMissingMark, i.Symbol)
}

// EquityCts is the balance plus the unrealised value of open positions.
func (a *Account) EquityCts(marks []Mark) (market.Cents, error) {
	balance, err := a.BalanceCts()
	if err != nil {
		return 0, err
	}
	unrealised, err := a.UnrealisedCts(marks)
	if err != nil {
		return 0, err
	}
	return market.AddCents(balance, unrealised)
}

// BalanceCts is the account's settled money: the starting balance plus
// realised P&L, minus fees. It contains nothing that depends on a mark, so
// unlike equity it cannot be moved by a price that has not been traded at.
func (a *Account) BalanceCts() (market.Cents, error) {
	balance, err := market.AddCents(a.startingCts, a.realisedCts)
	if err != nil {
		return 0, err
	}
	return market.SubCents(balance, a.feesCts)
}

// allocateCost is the cost basis removed when closing closeQty of openQty.
//
// It rounds toward positive infinity, which reduces the realised P&L of a
// long and of a short alike, so an inexact allocation always costs the trader.
// The residual basis is what is left after subtracting this, never a figure
// computed separately, so the allocations over any sequence of partial closes
// sum to exactly the original basis and no cent is created or lost.
func allocateCost(basisCts market.Cents, closeQty, openQty market.Qty) (market.Cents, error) {
	numerator, err := market.MulCents(basisCts, market.Cents(closeQty))
	if err != nil {
		return 0, err
	}
	denominator := market.Cents(openQty)
	q := numerator / denominator
	if numerator%denominator != 0 && numerator > 0 {
		q++
	}
	return q, nil
}

// opposed reports whether a signed fill reduces a position.
func opposed(net, signed market.Qty) bool {
	return net != 0 && ((net > 0) != (signed > 0))
}

// remainderAfterClose is the part of a signed fill left once closeQty has been
// closed, keeping the fill's direction.
func remainderAfterClose(signed, closeQty market.Qty) market.Qty {
	if signed > 0 {
		return signed - closeQty
	}
	return signed + closeQty
}

func sign(q market.Qty) int {
	if q > 0 {
		return 1
	}
	if q < 0 {
		return -1
	}
	return 0
}

func abs(q market.Qty) market.Qty {
	if q < 0 {
		return -q
	}
	return q
}

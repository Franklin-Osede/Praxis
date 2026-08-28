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
	if n := a.indexOf(i); n >= 0 {
		return a.positions[n], true
	}
	return Position{}, false
}

// indexOf returns the index of the instrument's position, or -1. Positions are
// kept sorted by symbol, so this scan is deterministic.
func (a *Account) indexOf(i market.Instrument) int {
	for n, p := range a.positions {
		if p.Instrument.Symbol == i.Symbol {
			return n
		}
	}
	return -1
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
func (a *Account) ApplyFill(f market.Fill) ([]PositionEvent, error) {
	if err := f.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidFill, err)
	}

	n := a.indexOf(f.Instrument)
	if n < 0 {
		n = a.openPosition(f.Instrument)
	}
	pos := a.positions[n]

	signed := f.SignedQty()
	var events []PositionEvent

	// Reduce first: an opposite-sided fill closes what it can before any
	// remainder opens a new position.
	if closing := opposed(pos.NetQty, signed); closing {
		closeQty := min(abs(signed), abs(pos.NetQty))

		removedCts := allocateCost(pos.CostBasisCts, closeQty, abs(pos.NetQty))
		proceedsCts := f.Instrument.Money(f.Price, market.Qty(sign(pos.NetQty))*closeQty)
		realisedCts := proceedsCts - removedCts

		pos.NetQty -= market.Qty(sign(pos.NetQty)) * closeQty
		pos.CostBasisCts -= removedCts
		a.realisedCts += realisedCts

		kind := PositionReduced
		if pos.NetQty == 0 {
			kind = PositionClosed
		}
		events = append(events, a.charge(PositionEvent{
			Kind: kind, Instrument: f.Instrument, Side: f.Side,
			Qty: closeQty, Price: f.Price, RealisedCts: realisedCts,
		}))

		signed = remainderAfterClose(signed, closeQty)
	}

	if signed != 0 {
		kind := PositionIncreased
		if pos.NetQty == 0 {
			kind = PositionOpened
		}
		pos.NetQty += signed
		pos.CostBasisCts += f.Instrument.Money(f.Price, signed)
		events = append(events, a.charge(PositionEvent{
			Kind: kind, Instrument: f.Instrument, Side: f.Side,
			Qty: abs(signed), Price: f.Price,
		}))
	}

	a.positions[n] = pos
	return events, nil
}

// charge applies the account's commission to one leg and records it.
func (a *Account) charge(e PositionEvent) PositionEvent {
	e.FeeCts = market.Cents(e.Qty) * a.commissionCts
	a.feesCts += e.FeeCts
	return e
}

// UnrealisedCts values every open position at the given marks.
func (a *Account) UnrealisedCts(marks []Mark) (market.Cents, error) {
	var total market.Cents
	for _, p := range a.positions {
		if p.IsFlat() {
			continue
		}
		mark, ok := markFor(marks, p.Instrument)
		if !ok {
			return 0, fmt.Errorf("%w: %s", ErrMissingMark, p.Instrument.Symbol)
		}
		total += p.UnrealisedCts(mark)
	}
	return total, nil
}

// markFor looks a mark up by symbol. Marks are an ordered slice and not a map
// so that a missing or duplicated mark behaves identically on every run.
func markFor(marks []Mark, i market.Instrument) (market.Ticks, bool) {
	for _, m := range marks {
		if m.Instrument.Symbol == i.Symbol {
			return m.Price, true
		}
	}
	return 0, false
}

// EquityCts is the starting balance plus realised money, minus fees, plus the
// unrealised value of open positions.
func (a *Account) EquityCts(marks []Mark) (market.Cents, error) {
	unrealised, err := a.UnrealisedCts(marks)
	if err != nil {
		return 0, err
	}
	return a.startingCts + a.realisedCts - a.feesCts + unrealised, nil
}

// allocateCost is the cost basis removed when closing closeQty of openQty.
//
// It rounds toward positive infinity, which reduces the realised P&L of a
// long and of a short alike, so an inexact allocation always costs the trader.
// The residual basis is what is left after subtracting this, never a figure
// computed separately, so the allocations over any sequence of partial closes
// sum to exactly the original basis and no cent is created or lost.
func allocateCost(basisCts market.Cents, closeQty, openQty market.Qty) market.Cents {
	numerator := basisCts * market.Cents(closeQty)
	denominator := market.Cents(openQty)
	q := numerator / denominator
	if numerator%denominator != 0 && numerator > 0 {
		q++
	}
	return q
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

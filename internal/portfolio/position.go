// Package portfolio holds what the trader owns and how it is protected. It
// imports no infrastructure.
//
// This package exists at the minimum needed by intrabar resolution, which must
// know a position's direction. Cost basis, realised and unrealised P&L and
// account equity arrive with Phase 1 and will grow these types.
package portfolio

import (
	"errors"

	"praxis/internal/market"
)

// Position is a net holding in one instrument.
type Position struct {
	Instrument market.Instrument

	// NetQty is signed: positive is long, negative is short, zero is flat.
	// This differs deliberately from Order and Fill quantities, which are
	// always positive and carry direction in their Side.
	NetQty market.Qty

	// CostBasisCts is the exact signed cash committed to the position:
	// positive for a long, negative for a short. It is never a rounded
	// average entry price, because averaging in integers loses precision on
	// every partial fill.
	CostBasisCts market.Cents
}

// UnrealisedCts is what the position would realise if closed at mark.
func (p Position) UnrealisedCts(mark market.Ticks) (market.Cents, error) {
	value, err := p.Instrument.Money(mark, p.NetQty)
	if err != nil {
		return 0, err
	}
	return market.SubCents(value, p.CostBasisCts)
}

// AvgPx is the average entry price, derived for display only. It must never
// feed a P&L or risk calculation: use CostBasisCts, which is exact.
func (p Position) AvgPx() market.Ticks {
	if p.IsFlat() || p.Instrument.CentsPerTick == 0 {
		return 0
	}
	return market.Ticks(p.CostBasisCts / (p.Instrument.CentsPerTick * market.Cents(p.NetQty)))
}

func (p Position) IsLong() bool  { return p.NetQty > 0 }
func (p Position) IsShort() bool { return p.NetQty < 0 }
func (p Position) IsFlat() bool  { return p.NetQty == 0 }

// ExitSide is the side an order must take to reduce the position.
func (p Position) ExitSide() market.Side {
	if p.IsLong() {
		return market.SideSell
	}
	if p.IsShort() {
		return market.SideBuy
	}
	return market.SideUnspecified
}

// Errors reported when a position is not internally coherent.
var (
	ErrFlatWithCostBasis    = errors.New("portfolio: flat position holds a cost basis")
	ErrOpenWithoutCostBasis = errors.New("portfolio: open position holds no cost basis")
	ErrCostBasisSign        = errors.New("portfolio: cost basis sign contradicts the position direction")
)

// Validate reports why the position is not a valid domain value, or nil.
//
// A flat position is valid: a position that closes to zero stays flat rather
// than being deleted. It must, however, hold no cost basis, and an open one
// must hold a basis whose sign matches its direction—a long commits cash and a
// short receives it. That sign relation holds only because Praxis requires
// strictly positive prices; see the specification before relaxing either.
func (p Position) Validate() error {
	if err := p.Instrument.Validate(); err != nil {
		return err
	}
	switch {
	case p.IsFlat() && p.CostBasisCts != 0:
		return ErrFlatWithCostBasis
	case !p.IsFlat() && p.CostBasisCts == 0:
		return ErrOpenWithoutCostBasis
	case p.IsLong() && p.CostBasisCts < 0:
		return ErrCostBasisSign
	case p.IsShort() && p.CostBasisCts > 0:
		return ErrCostBasisSign
	}
	return nil
}

// ProtectiveLevels are the stop and target attached to a position. A zero
// price means the level is not set, consistent with order prices.
type ProtectiveLevels struct {
	Stop   market.Ticks
	Target market.Ticks
}

func (l ProtectiveLevels) HasStop() bool   { return l.Stop != 0 }
func (l ProtectiveLevels) HasTarget() bool { return l.Target != 0 }

// Errors reported when protective levels cannot describe a real position.
var (
	ErrLevelsOnFlatPosition = errors.New("portfolio: protective levels on a flat position")
	ErrInvertedLevels       = errors.New("portfolio: stop and target are on the wrong sides of the position")
)

// ValidateFor reports why the levels cannot protect the given position, or
// nil. A long is protected by a stop below its target and a short by a stop
// above it; the reverse is a configuration error, not an exotic strategy.
func (l ProtectiveLevels) ValidateFor(p Position) error {
	if p.IsFlat() {
		if l.HasStop() || l.HasTarget() {
			return ErrLevelsOnFlatPosition
		}
		return nil
	}
	if !l.HasStop() || !l.HasTarget() {
		return nil
	}
	if p.IsLong() && l.Stop >= l.Target {
		return ErrInvertedLevels
	}
	if p.IsShort() && l.Stop <= l.Target {
		return ErrInvertedLevels
	}
	return nil
}

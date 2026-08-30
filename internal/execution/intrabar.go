package execution

import (
	"errors"
	"fmt"

	"praxis/internal/market"
	"praxis/internal/portfolio"
)

// IntrabarOutcome names which protective level a bar resolved to.
type IntrabarOutcome uint8

const (
	IntrabarNone IntrabarOutcome = iota
	IntrabarStop
	IntrabarTarget
)

func (o IntrabarOutcome) String() string {
	switch o {
	case IntrabarStop:
		return "stop"
	case IntrabarTarget:
		return "target"
	default:
		return "none"
	}
}

// IntrabarResult is what a bar did to a protected position.
type IntrabarResult struct {
	Outcome IntrabarOutcome

	// Price is the exit price, zero when Outcome is IntrabarNone.
	Price market.Ticks

	// Ambiguous records that the bar reached both levels and did not say in
	// which order, so the policy chose. It is a fact about the observation,
	// not about the policy, and is false when the bar's own open settles the
	// order of events.
	Ambiguous bool

	// Gapped records that the bar opened beyond the resolved level, so the
	// fill is worse than the level rather than at it.
	Gapped bool
}

// Errors reported when input cannot describe a real bar resolution.
var (
	ErrInvalidBar      = errors.New("execution: bar is not a valid domain value")
	ErrInvalidPosition = errors.New("execution: position is not a valid domain value")
	ErrInvalidLevels   = errors.New("execution: protective levels do not describe the position")
)

// IntrabarResolutionPolicy decides what a completed bar did to a protected
// position. Which levels a bar reached is an observation; the order in which
// it reached them is usually unknowable, and choosing is a simulation policy.
// See ADR-007.
type IntrabarResolutionPolicy interface {
	Resolve(bar market.Bar, pos portfolio.Position, lv portfolio.ProtectiveLevels) (IntrabarResult, error)
}

// WorstCaseIntrabar resolves an ambiguous bar against the trader.
type WorstCaseIntrabar struct{}

// Resolve reports what the bar did to the position.
//
// A position's protective levels are orders on the exit side: the stop is a
// stop order and the target is a limit order. Whether a level was reached is
// therefore decided by the same predicates as quote execution, applied to the
// extreme of the bar that could have reached it.
//
// The open is the first price the bar showed. A level the open is already
// beyond was reached before anything else in the interval, so the bar is not
// ambiguous and the stop fills at the open rather than at its level. Only when
// the open sits between the levels, and the bar later reaches both, is the
// order of events unknowable—and then the stop wins.
//
// The kernel does not infer the path between bars: see ADR-010.
func (WorstCaseIntrabar) Resolve(bar market.Bar, pos portfolio.Position, lv portfolio.ProtectiveLevels) (IntrabarResult, error) {
	if err := bar.Validate(); err != nil {
		return IntrabarResult{}, fmt.Errorf("%w: %w", ErrInvalidBar, err)
	}
	if err := pos.Validate(); err != nil {
		return IntrabarResult{}, fmt.Errorf("%w: %w", ErrInvalidPosition, err)
	}
	if pos.Instrument != bar.Instrument {
		return IntrabarResult{}, ErrInstrumentMismatch
	}
	if err := lv.ValidateFor(pos); err != nil {
		return IntrabarResult{}, fmt.Errorf("%w: %w", ErrInvalidLevels, err)
	}
	if pos.IsFlat() {
		return IntrabarResult{}, nil
	}

	exit := pos.ExitSide()

	// A long exits by selling, so its stop is reached by the low and its
	// target by the high. A short is the mirror.
	stopExtreme, targetExtreme := bar.Low, bar.High
	if exit == market.SideBuy {
		stopExtreme, targetExtreme = bar.High, bar.Low
	}

	stopReached := lv.HasStop() && reachedStop(exit, stopExtreme, lv.Stop)
	targetReached := lv.HasTarget() && reachedLimit(exit, targetExtreme, lv.Target)

	if stopReached && reachedStop(exit, bar.Open, lv.Stop) {
		return IntrabarResult{Outcome: IntrabarStop, Price: bar.Open, Gapped: bar.Open != lv.Stop}, nil
	}
	if targetReached && reachedLimit(exit, bar.Open, lv.Target) {
		return IntrabarResult{Outcome: IntrabarTarget, Price: lv.Target, Gapped: bar.Open != lv.Target}, nil
	}

	switch {
	case stopReached && targetReached:
		return IntrabarResult{Outcome: IntrabarStop, Price: lv.Stop, Ambiguous: true}, nil
	case stopReached:
		return IntrabarResult{Outcome: IntrabarStop, Price: lv.Stop}, nil
	case targetReached:
		return IntrabarResult{Outcome: IntrabarTarget, Price: lv.Target}, nil
	}
	return IntrabarResult{}, nil
}

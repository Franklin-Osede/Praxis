package session

import (
	"errors"
	"fmt"

	"praxis/internal/challenge"
	"praxis/internal/market"
	"praxis/internal/portfolio"
)

// Errors reported when a journal cannot be reconstructed or does not agree
// with itself.
var (
	ErrNoSessionStarted   = errors.New("session: the log does not begin with a session start")
	ErrContradictoryLog   = errors.New("session: a recorded context contradicts the events before it")
	ErrUnexpectedSequence = errors.New("session: the log is not one contiguous ordering")
)

// Replay rebuilds an account and an evaluation from a journal alone.
//
// It consumes only the facts that carry state: the configuration, the fills,
// the boundaries and the valuations. Everything else in the log is a record of
// what happened, not an input to it — which is the point of the exercise. If
// reconstruction needed an observation or a decision event, the log would be
// describing state it did not contain.
func Replay(events []Event) (*portfolio.Account, *challenge.Challenge, error) {
	if len(events) == 0 {
		return nil, nil, ErrNoSessionStarted
	}
	started, ok := events[0].(SessionStarted)
	if !ok {
		return nil, nil, ErrNoSessionStarted
	}

	account, err := portfolio.NewAccount(started.Config.StartingBalanceCts, started.Config.CommissionPerContractCts)
	if err != nil {
		return nil, nil, err
	}
	eval, err := challenge.New(started.Config.Rules)
	if err != nil {
		return nil, nil, err
	}

	for n, e := range events {
		if got, want := e.Header().Sequence, uint64(n+1); got != want {
			return nil, nil, fmt.Errorf("%w: event %d carries sequence %d", ErrUnexpectedSequence, n, got)
		}
		switch v := e.(type) {
		case FillProduced:
			if _, err := account.ApplyFill(v.Fill); err != nil {
				return nil, nil, err
			}
		case SessionOpened:
			if _, err := eval.OpenSession(challenge.SessionOpened{
				Time: v.Time, Sequence: v.Sequence, SessionID: v.SessionID,
				BalanceCts: v.BalanceCts, EquityCts: v.EquityCts,
			}); err != nil {
				return nil, nil, err
			}
		case AccountValued:
			if _, err := eval.Observe(challenge.AccountSnapshot{
				Time: v.Time, Sequence: v.Sequence, SessionID: v.SessionID,
				BalanceCts: v.BalanceCts, EquityCts: v.EquityCts,
			}); err != nil {
				return nil, nil, err
			}
		}
	}
	return account, eval, nil
}

// Verify reports whether the derived context recorded on every decision agrees
// with the events that precede it.
//
// A behavioural log stores figures that could also be computed from the
// stream. That duplication is deliberate — a decision must carry the state it
// was taken in — but it means the log can hold two contradictory truths, and
// nothing else would notice. This is what stops that.
func Verify(events []Event) error {
	var (
		trades      uint32
		losses      uint32
		realisedCts market.Cents
		netQty      market.Qty
		balanceCts  market.Cents
		equityCts   market.Cents
		valued      bool
	)

	for _, e := range events {
		switch v := e.(type) {
		case SessionOpened:
			trades, losses, realisedCts = 0, 0, 0

		case AccountValued:
			balanceCts, equityCts, valued = v.BalanceCts, v.EquityCts, true

		case PositionChanged:
			netQty += signedQty(v.Change.Side, v.Change.Qty)
			if v.Change.Kind == portfolio.PositionReduced || v.Change.Kind == portfolio.PositionClosed {
				realisedCts += v.Change.RealisedCts
				if v.Change.RealisedCts < 0 {
					losses++
				} else {
					losses = 0
				}
			}

		case OrderSubmitted:
			want := OrderContext{
				BalanceCts:         balanceCts,
				EquityCts:          equityCts,
				TradesThisSession:  trades,
				ConsecutiveLosses:  losses,
				SessionRealisedCts: realisedCts,
				PositionQtyBefore:  netQty,
			}
			if !valued {
				return fmt.Errorf("%w: order %s was submitted before any valuation", ErrContradictoryLog, v.Order.ID)
			}
			if v.Context != want {
				return fmt.Errorf("%w: order %s recorded %+v, the events before it say %+v",
					ErrContradictoryLog, v.Order.ID, v.Context, want)
			}
			trades++
		}
	}
	return nil
}

func signedQty(side market.Side, qty market.Qty) market.Qty {
	if side == market.SideSell {
		return -qty
	}
	return qty
}

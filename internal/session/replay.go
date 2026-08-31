package session

import (
	"errors"
	"fmt"
	"math"

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
	ErrCounterOverflow    = errors.New("session: a counter in the log cannot be represented")
)

// ReplayedState is everything a session needs to carry on from a journal:
// the aggregates, the trading session it was in, the last book it saw, and the
// behavioural counters that a decision's context is measured against.
type ReplayedState struct {
	Config    Config
	Account   *portfolio.Account
	Challenge *challenge.Challenge

	SessionOpen         bool
	CurrentSessionID    challenge.SessionID
	LastQuote           market.Quote
	HasQuote            bool
	ObservedThisSession bool

	OrdersThisSession  uint32
	ConsecutiveLosses  uint32
	SessionRealisedCts market.Cents

	LastSequence uint64
	Events       []Event
}

// Replay rebuilds a session's whole state from its journal alone.
//
// The aggregates are rebuilt from the facts that carry state: the
// configuration, the fills, the boundaries and the valuations. Everything else
// in the log is a record of what happened rather than an input to it — except
// for the observations and the position changes, which carry no state into the
// aggregates but do carry the session's own: which book was last seen, and
// what the behavioural counters stood at.
func Replay(events []Event) (*ReplayedState, error) {
	if len(events) == 0 {
		return nil, ErrNoSessionStarted
	}
	started, ok := events[0].(SessionStarted)
	if !ok {
		return nil, ErrNoSessionStarted
	}

	account, err := portfolio.NewAccount(started.Config.StartingBalanceCts, started.Config.CommissionPerContractCts)
	if err != nil {
		return nil, err
	}
	eval, err := challenge.New(started.Config.Rules)
	if err != nil {
		return nil, err
	}

	state := &ReplayedState{Config: started.Config, Account: account, Challenge: eval}
	for n, e := range events {
		if got, want := e.Header().Sequence, uint64(n+1); got != want {
			return nil, fmt.Errorf("%w: event %d carries sequence %d", ErrUnexpectedSequence, n, got)
		}
		state.LastSequence = e.Header().Sequence

		switch v := e.(type) {
		case FillProduced:
			if _, err := account.ApplyFill(v.Fill); err != nil {
				return nil, err
			}

		case SessionOpened:
			if _, err := eval.OpenSession(challenge.SessionOpened{
				Time: v.Time, Sequence: v.Sequence, SessionID: v.SessionID,
				BalanceCts: v.BalanceCts, EquityCts: v.EquityCts,
			}); err != nil {
				return nil, err
			}
			state.SessionOpen, state.CurrentSessionID = true, v.SessionID
			state.ObservedThisSession = false
			state.OrdersThisSession, state.ConsecutiveLosses, state.SessionRealisedCts = 0, 0, 0

		case SessionEnded:
			state.SessionOpen, state.ObservedThisSession = false, false

		case AccountValued:
			if _, err := eval.Observe(challenge.AccountSnapshot{
				Time: v.Time, Sequence: v.Sequence, SessionID: v.SessionID,
				BalanceCts: v.BalanceCts, EquityCts: v.EquityCts,
			}); err != nil {
				return nil, err
			}

		case MarketObserved:
			state.LastQuote, state.HasQuote, state.ObservedThisSession = v.Quote, true, true

		case OrderSubmitted:
			if state.OrdersThisSession, err = addOrders(state.OrdersThisSession, 1); err != nil {
				return nil, err
			}

		case PositionChanged:
			if v.Change.Kind != portfolio.PositionReduced && v.Change.Kind != portfolio.PositionClosed {
				continue
			}
			if state.SessionRealisedCts, err = market.AddCents(state.SessionRealisedCts, v.Change.RealisedCts); err != nil {
				return nil, err
			}
			if v.Change.RealisedCts < 0 {
				if state.ConsecutiveLosses, err = addOrders(state.ConsecutiveLosses, 1); err != nil {
					return nil, err
				}
			} else {
				state.ConsecutiveLosses = 0
			}
		}
	}

	state.Events = make([]Event, len(events))
	copy(state.Events, events)
	return state, nil
}

// Resume continues a replayed session, so that the next command produces the
// same event a session that was never interrupted would have produced.
func Resume(state *ReplayedState) (*Session, error) {
	journal, err := newJournalFrom(state.Events)
	if err != nil {
		return nil, err
	}
	return &Session{
		cfg:                 state.Config,
		account:             state.Account,
		eval:                state.Challenge,
		journal:             journal,
		sequence:            state.LastSequence,
		lastQuote:           state.LastQuote,
		hasQuote:            state.HasQuote,
		openSessionID:       state.CurrentSessionID,
		sessionOpen:         state.SessionOpen,
		observedThisSession: state.ObservedThisSession,
		ordersThisSession:   state.OrdersThisSession,
		consecutiveLosses:   state.ConsecutiveLosses,
		sessionRealisedCts:  state.SessionRealisedCts,
	}, nil
}

// Verify reports whether the derived context recorded on every decision agrees
// with the events that precede it.
//
// A behavioural log stores figures that could also be computed from the
// stream. That duplication is deliberate — a decision must carry the state it
// was taken in — but it means the log can hold two contradictory truths, and
// nothing else would notice. This is what stops that.
//
// Its own arithmetic is checked. A verifier that silently wrapped would accept
// a corrupt log for exactly the reason it exists to reject one.
func Verify(events []Event) error {
	var (
		orders      uint32
		losses      uint32
		realisedCts market.Cents
		netQty      market.Qty
		balanceCts  market.Cents
		equityCts   market.Cents
		valued      bool
		err         error
	)

	for _, e := range events {
		switch v := e.(type) {
		case SessionOpened:
			// A new session's counters reset, and so does the valuation. A
			// session that opened without being valued must not let the
			// previous session's figures validate one of its decisions.
			orders, losses, realisedCts, valued = 0, 0, 0, false

		case AccountValued:
			balanceCts, equityCts, valued = v.BalanceCts, v.EquityCts, true

		case PositionChanged:
			if netQty, err = market.AddQty(netQty, signedQty(v.Change.Side, v.Change.Qty)); err != nil {
				return err
			}
			if v.Change.Kind != portfolio.PositionReduced && v.Change.Kind != portfolio.PositionClosed {
				continue
			}
			if realisedCts, err = market.AddCents(realisedCts, v.Change.RealisedCts); err != nil {
				return err
			}
			if v.Change.RealisedCts < 0 {
				if losses, err = addOrders(losses, 1); err != nil {
					return err
				}
			} else {
				losses = 0
			}

		case OrderSubmitted:
			if !valued {
				return fmt.Errorf("%w: order %s was submitted with no valuation in its session",
					ErrContradictoryLog, v.Order.ID)
			}
			want := OrderContext{
				BalanceCts:                 balanceCts,
				EquityCts:                  equityCts,
				OrdersSubmittedThisSession: orders,
				ConsecutiveLosses:          losses,
				SessionRealisedCts:         realisedCts,
				PositionQtyBefore:          netQty,
			}
			if v.Context != want {
				return fmt.Errorf("%w: order %s recorded %+v, the events before it say %+v",
					ErrContradictoryLog, v.Order.ID, v.Context, want)
			}
			if orders, err = addOrders(orders, 1); err != nil {
				return err
			}
		}
	}
	return nil
}

func addOrders(n, by uint32) (uint32, error) {
	if n > math.MaxUint32-by {
		return 0, fmt.Errorf("%w: %d + %d", ErrCounterOverflow, n, by)
	}
	return n + by, nil
}

func signedQty(side market.Side, qty market.Qty) market.Qty {
	if side == market.SideSell {
		return -qty
	}
	return qty
}

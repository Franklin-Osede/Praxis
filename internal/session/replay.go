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

	// ErrFabricated reports a recorded fact the aggregates did not produce.
	// A journal is not a source of truth because it is well formed; it is one
	// because every derived fact in it can be rebuilt from the facts that
	// caused it and found identical.
	ErrFabricated = errors.New("session: a recorded fact is not what the aggregates produced")

	// ErrStructure reports a log that does not describe a session at all:
	// an event whose kind contradicts its type, time running backwards, or a
	// trading session opened, valued or ended out of turn.
	ErrStructure = errors.New("session: the log's structure is invalid")
)

// kindOf is the kind an event must declare. A payload and its tag disagreeing
// is the first thing a serialiser will get wrong.
func kindOf(e Event) Kind {
	switch e.(type) {
	case SessionStarted:
		return KindSessionStarted
	case SessionOpened:
		return KindSessionOpened
	case MarketObserved:
		return KindMarketObserved
	case OrderSubmitted:
		return KindOrderSubmitted
	case FillProduced:
		return KindFillProduced
	case PositionChanged:
		return KindPositionChanged
	case AccountValued:
		return KindAccountValued
	case ChallengeDecision:
		return KindChallengeDecision
	case SessionEnded:
		return KindSessionEnded
	default:
		return 0
	}
}

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

// Replay rebuilds a session's whole state from its journal, and proves the
// journal while doing it.
//
// Every derived fact is recomputed from the facts that caused it and compared
// exactly: an account valuation against the account and the last book, a
// position change against what applying the fill actually produced, a
// challenge decision against what the evaluation actually decided. A log that
// merely reads consistently is not enough — a fabricated equity could record
// an evaluation failing where no account could have failed, and a fabricated
// realised amount could be made internally coherent by adjusting every context
// after it.
//
// It also checks the log describes a session at all: each event's kind matches
// its type, time never runs backwards, sequences are contiguous, and trading
// sessions open, are valued and end in turn under their own identifiers.
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
	var (
		lastTime         market.LogicalTime
		pendingChanges   []portfolio.PositionEvent
		pendingDecisions []challenge.Event
	)

	for n, e := range events {
		header := e.Header()
		if got, want := header.Sequence, uint64(n+1); got != want {
			return nil, fmt.Errorf("%w: event %d carries sequence %d", ErrUnexpectedSequence, n, got)
		}
		if got, want := header.Kind, kindOf(e); got != want {
			return nil, fmt.Errorf("%w: event %d is a %T tagged %v", ErrStructure, n, e, got)
		}
		if header.Time < lastTime {
			return nil, fmt.Errorf("%w: event %d moves time back to %d", ErrStructure, n, header.Time)
		}
		lastTime = header.Time

		if _, ok := e.(PositionChanged); !ok && len(pendingChanges) > 0 {
			return nil, fmt.Errorf("%w: event %d follows a fill whose %d changes were not recorded",
				ErrFabricated, n, len(pendingChanges))
		}
		if _, ok := e.(ChallengeDecision); !ok && len(pendingDecisions) > 0 {
			return nil, fmt.Errorf("%w: event %d follows %d unrecorded challenge decisions",
				ErrFabricated, n, len(pendingDecisions))
		}
		state.LastSequence = header.Sequence

		switch v := e.(type) {
		case SessionStarted:
			if n != 0 {
				return nil, fmt.Errorf("%w: a second session start at event %d", ErrStructure, n)
			}

		case FillProduced:
			pendingChanges, err = account.ApplyFill(v.Fill)
			if err != nil {
				return nil, err
			}

		case PositionChanged:
			if len(pendingChanges) == 0 {
				return nil, fmt.Errorf("%w: event %d is a position change no fill produced", ErrFabricated, n)
			}
			if pendingChanges[0] != v.Change {
				return nil, fmt.Errorf("%w: event %d records %+v, applying the fill produced %+v",
					ErrFabricated, n, v.Change, pendingChanges[0])
			}
			pendingChanges = pendingChanges[1:]
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

		case SessionOpened:
			if state.SessionOpen {
				return nil, fmt.Errorf("%w: event %d opens %s while %s is open", ErrStructure, n, v.SessionID, state.CurrentSessionID)
			}
			if err := checkValuation(account, started.Config.Instrument, state, v.BalanceCts, v.EquityCts, n); err != nil {
				return nil, err
			}
			pendingDecisions, err = eval.OpenSession(challenge.SessionOpened{
				Time: v.Time, Sequence: v.Sequence, SessionID: v.SessionID,
				BalanceCts: v.BalanceCts, EquityCts: v.EquityCts,
			})
			if err != nil {
				return nil, err
			}
			state.SessionOpen, state.CurrentSessionID = true, v.SessionID
			state.ObservedThisSession = false
			state.OrdersThisSession, state.ConsecutiveLosses, state.SessionRealisedCts = 0, 0, 0

		case SessionEnded:
			if !state.SessionOpen || v.SessionID != state.CurrentSessionID {
				return nil, fmt.Errorf("%w: event %d ends %s, which is not the open session", ErrStructure, n, v.SessionID)
			}
			state.SessionOpen, state.ObservedThisSession = false, false

		case AccountValued:
			if !state.SessionOpen || v.SessionID != state.CurrentSessionID {
				return nil, fmt.Errorf("%w: event %d values %s, which is not the open session", ErrStructure, n, v.SessionID)
			}
			if err := checkValuation(account, started.Config.Instrument, state, v.BalanceCts, v.EquityCts, n); err != nil {
				return nil, err
			}
			pendingDecisions, err = eval.Observe(challenge.AccountSnapshot{
				Time: v.Time, Sequence: v.Sequence, SessionID: v.SessionID,
				BalanceCts: v.BalanceCts, EquityCts: v.EquityCts,
			})
			if err != nil {
				return nil, err
			}

		case ChallengeDecision:
			if len(pendingDecisions) == 0 {
				return nil, fmt.Errorf("%w: event %d is a decision the evaluation did not make", ErrFabricated, n)
			}
			want := pendingDecisions[0]
			if v.Decision != want.Decision || v.CausedBySequence != want.Sequence {
				return nil, fmt.Errorf("%w: event %d records %+v caused by %d, the evaluation decided %+v caused by %d",
					ErrFabricated, n, v.Decision, v.CausedBySequence, want.Decision, want.Sequence)
			}
			pendingDecisions = pendingDecisions[1:]

		case MarketObserved:
			state.LastQuote, state.HasQuote, state.ObservedThisSession = v.Quote, true, true

		case OrderSubmitted:
			if state.OrdersThisSession, err = addOrders(state.OrdersThisSession, 1); err != nil {
				return nil, err
			}
		}
	}

	if len(pendingChanges) > 0 || len(pendingDecisions) > 0 {
		return nil, fmt.Errorf("%w: the log ends with %d position changes and %d decisions unrecorded",
			ErrFabricated, len(pendingChanges), len(pendingDecisions))
	}

	state.Events = make([]Event, len(events))
	copy(state.Events, events)
	return state, nil
}

// checkValuation rebuilds a recorded valuation from the account and the last
// book, and refuses one the account could not have produced.
func checkValuation(a *portfolio.Account, i market.Instrument, state *ReplayedState, balanceCts, equityCts market.Cents, n int) error {
	wantBalance, wantEquity, err := valueAccount(a, i, state.LastQuote, state.HasQuote)
	if err != nil {
		return err
	}
	if balanceCts != wantBalance || equityCts != wantEquity {
		return fmt.Errorf("%w: event %d records balance %d equity %d, the account holds %d and %d",
			ErrFabricated, n, balanceCts, equityCts, wantBalance, wantEquity)
	}
	return nil
}

// Resume continues a replayed session, so that the next command produces the
// same event a session that was never interrupted would have produced.
//
// It builds a new session rather than healing one: a session that failed to
// commit is discarded, never repaired, because nothing inside it can know what
// reached the disk.
func Resume(state *ReplayedState, committer BatchCommitter) (*Session, error) {
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
		committer:           committer,
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

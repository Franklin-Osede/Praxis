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
	ErrEpisode            = errors.New("session: position changes do not describe an episode")
	ErrProtectionRef      = errors.New("session: a protection reference names neither an entry nor an episode")

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
	case OrderRested:
		return KindOrderRested
	case OrderCancelled:
		return KindOrderCancelled
	case ProtectionPlaced:
		return KindProtectionPlaced
	case ProtectionReplaced:
		return KindProtectionReplaced
	case ProtectionEnded:
		return KindProtectionEnded
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

	// ConsecutiveLosingTrades is the streak of completed losing episodes, which
	// a resumed session must continue rather than restart.
	ConsecutiveLosingTrades uint32

	// episodes carries the whole projection, not only its counter: an episode
	// open when a run was interrupted must still be open when it resumes, or
	// the close that ends it would arrive with nothing to end.
	episodes episodeProjection

	// PlannedProtections are the protections whose entries have not filled,
	// ActiveProtections those bound to an open episode, and UsedOrderIDs every
	// identifier the journal has spent — never reused, so a resumed session
	// must carry them.
	PlannedProtections []PlannedProtection
	ActiveProtections  []ActiveProtection
	UsedOrderIDs       []string

	// Working is the set of orders waiting for a later observation, in the
	// order they were submitted. A session that resumed without it would
	// forget a stop the trader believed was protecting them.
	Working []market.Order

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
		episodes         episodeProjection
		protections      protectionProjection
		lastTime         market.LogicalTime
		pendingChanges   []portfolio.PositionEvent
		pendingDecisions []challenge.Event

		// fillOrderID is the order whose fill the position changes now being
		// read belong to. A change does not name it, and protection binds to
		// the decision that opened the exposure, not to the exposure alone.
		fillOrderID string
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

		if _, ok := e.(PositionChanged); !ok && len(pendingChanges) > 0 && !owedConsequenceStandsHere(e, &protections) {
			return nil, fmt.Errorf("%w: event %d follows a fill whose %d changes were not recorded",
				ErrFabricated, n, len(pendingChanges))
		}
		if _, ok := e.(ChallengeDecision); !ok && len(pendingDecisions) > 0 {
			return nil, fmt.Errorf("%w: event %d follows %d unrecorded challenge decisions",
				ErrFabricated, n, len(pendingDecisions))
		}
		if _, err := protections.requireOwed(e); err != nil {
			return nil, fmt.Errorf("%w: event %d: %v", ErrFabricated, n, err)
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
			fillOrderID = v.Fill.OrderID
			// A fill from an order that never rested matches nothing here,
			// which is correct: it was gone before it could wait.
			if state.Working, err = reduceWorking(state.Working, v.Fill.OrderID, v.Fill.Qty); err != nil {
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
			// The episode projection runs on changes already proved against
			// what applying the fill produced, so what it derives rests on
			// facts rather than on the log's word for them.
			symbol := v.Change.Instrument.Symbol
			episodeID, _ := episodes.episodeID(symbol)
			if err := episodes.apply(v.Sequence, v.Change); err != nil {
				return nil, err
			}
			if v.Change.Kind == portfolio.PositionOpened {
				episodeID, _ = episodes.episodeID(symbol)
			}
			// What is left of this fill's effect says whether a close is a
			// reversal, which is the one thing the ending of a protection
			// turns on and the one thing a reader without the fills — Verify —
			// cannot see.
			flip := v.Change.Kind == portfolio.PositionClosed && len(pendingChanges) > 0
			net := episodes.netQtyOf(symbol)
			protections.owe(protections.consequencesOf(fillOrderID, episodeID, v.Change, net, flip, true)...)
			protections.bind(fillOrderID, episodeID, v.Change, net)

			state.ConsecutiveLosingTrades = episodes.consecutiveLosingTradesNow()
			state.episodes = episodes
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

		case OrderRested:
			resting := v.Order
			resting.Qty = v.RestingQty
			state.Working = append(state.Working, resting)

		case OrderCancelled:
			state.Working = removeWorking(state.Working, v.OrderID)
			protections.entryGone(v.OrderID)
			protections.legCancelled(v.OrderID)

		case OrderSubmitted:
			if state.OrdersThisSession, err = addOrders(state.OrdersThisSession, 1); err != nil {
				return nil, err
			}
			if err := protections.claim(v.Order.ID); err != nil {
				return nil, fmt.Errorf("%w: event %d: %v", ErrFabricated, n, err)
			}

		case ProtectionPlaced:
			if err := protections.applyPlaced(v); err != nil {
				return nil, fmt.Errorf("%w: event %d: %v", ErrFabricated, n, err)
			}

		case ProtectionReplaced:
			if err := protections.applyReplaced(v); err != nil {
				return nil, fmt.Errorf("%w: event %d: %v", ErrFabricated, n, err)
			}

		case ProtectionEnded:
			if err := protections.applyEnded(v); err != nil {
				return nil, fmt.Errorf("%w: event %d: %v", ErrFabricated, n, err)
			}
		}
	}

	if len(pendingChanges) > 0 || len(pendingDecisions) > 0 {
		return nil, fmt.Errorf("%w: the log ends with %d position changes and %d decisions unrecorded",
			ErrFabricated, len(pendingChanges), len(pendingDecisions))
	}
	if err := protections.settled(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFabricated, err)
	}

	state.PlannedProtections = protections.snapshot()
	state.ActiveProtections = protections.activeSnapshot()
	state.UsedOrderIDs = protections.usedIdentifiers()
	state.Events = make([]Event, len(events))
	copy(state.Events, events)
	return state, nil
}

// owedConsequenceStandsHere allows the only things that may come between two
// changes of a single fill: what those changes require.
//
// A reversal reads close, the old protection's legs cancelled, the old
// protection ended, then open — because what the close undoes must be gone
// before what the open activates begins. Nothing else may stand there, and
// which consequence is owed is settled immediately afterwards.
//
// The second half of the condition is defence in depth: refusing a consequence
// nobody owes cannot be shown by forgery, because a journal that puts one there
// has to displace something else, and the displacement is caught first.
func owedConsequenceStandsHere(e Event, protections *protectionProjection) bool {
	if len(protections.owed) == 0 {
		return false
	}
	switch e.(type) {
	case ProtectionEnded, OrderCancelled:
		return true
	}
	return false
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
	resumed := &Session{
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
		episodes:            state.episodes,
		sessionRealisedCts:  state.SessionRealisedCts,
		working:             restoreWorking(state.Working),
		committer:           committer,
	}
	resumed.protections.restore(state.PlannedProtections, state.ActiveProtections,
		state.Config.Instrument.Symbol, state.UsedOrderIDs)
	return resumed, nil
}

func restoreWorking(orders []market.Order) []workingOrder {
	out := make([]workingOrder, 0, len(orders))
	for _, o := range orders {
		full := o
		out = append(out, workingOrder{order: full, remaining: o.Qty})
	}
	return out
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

		// The same projections the live session and Replay use. A second
		// implementation would eventually disagree with the first, and the
		// disagreement would be between a journal and the thing checking it.
		episodes    episodeProjection
		protections protectionProjection

		// sides remembers which way an entry would open, which is what makes a
		// stop's move away from it a widening.
		sides = map[string]bool{}

		// fillOrderID is the order whose fill the position changes now being
		// read belong to.
		fillOrderID string
	)

	for _, e := range events {
		owedThis, owedErr := protections.requireOwed(e)
		if owedErr != nil {
			return fmt.Errorf("%w: %v", ErrContradictoryLog, owedErr)
		}

		switch v := e.(type) {
		case SessionOpened:
			// A new session's counters reset, and so does the valuation. A
			// session that opened without being valued must not let the
			// previous session's figures validate one of its decisions.
			orders, losses, realisedCts, valued = 0, 0, 0, false

		case AccountValued:
			balanceCts, equityCts, valued = v.BalanceCts, v.EquityCts, true

		case FillProduced:
			fillOrderID = v.Fill.OrderID

		case PositionChanged:
			if netQty, err = market.AddQty(netQty, signedQty(v.Change.Side, v.Change.Qty)); err != nil {
				return err
			}
			symbol := v.Change.Instrument.Symbol
			episodeID, _ := episodes.episodeID(symbol)
			if err := episodes.apply(v.Sequence, v.Change); err != nil {
				return err
			}
			if v.Change.Kind == portfolio.PositionOpened {
				episodeID, _ = episodes.episodeID(symbol)
			}
			// Verify has the events and not the fills, so it cannot tell a
			// reversal from an exit — the one thing an ending's reason turns
			// on. It demands the same consequences with that reason unchecked,
			// which is a prefix of what Replay demands.
			net := episodes.netQtyOf(symbol)
			protections.owe(protections.consequencesOf(fillOrderID, episodeID, v.Change, net, false, false)...)
			protections.bind(fillOrderID, episodeID, v.Change, net)
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
				ConsecutiveLosingTrades:    episodes.consecutiveLosingTradesNow(),
			}
			if v.Context != want {
				return fmt.Errorf("%w: order %s recorded %+v, the events before it say %+v",
					ErrContradictoryLog, v.Order.ID, v.Context, want)
			}
			if orders, err = addOrders(orders, 1); err != nil {
				return err
			}
			sides[v.Order.ID] = v.Order.Side == market.SideBuy

		case OrderCancelled:
			protections.entryGone(v.OrderID)
			protections.legCancelled(v.OrderID)

		case ProtectionPlaced:
			if err := protections.applyPlaced(v); err != nil {
				return fmt.Errorf("%w: %v", ErrContradictoryLog, err)
			}

		case ProtectionReplaced:
			current, ok := protections.levelsFor(v.Ref)
			if !ok {
				return fmt.Errorf("%w: a change to a protection that does not exist", ErrContradictoryLog)
			}
			// The previous levels are recorded, so they are checked rather
			// than believed, and Widened is derived, so it is recomputed.
			if v.PreviousStopPrice != current.stopPrice || v.PreviousTargetPrice != current.targetPrice {
				return fmt.Errorf("%w: a change records %d/%d as the previous levels, the events before it say %d/%d",
					ErrContradictoryLog, v.PreviousStopPrice, v.PreviousTargetPrice,
					current.stopPrice, current.targetPrice)
			}
			// A planned protection's direction is the side its entry would
			// open; an active one carries its own, because the entry that
			// placed it may be long finished.
			long := current.long
			if v.Ref.Kind == ProtectionRefEntry {
				long = sides[v.Ref.OrderID]
			}
			if want := widened(long, current.stopPrice, v.StopPrice); v.Widened != want {
				return fmt.Errorf("%w: a change records widened=%v, the levels say %v",
					ErrContradictoryLog, v.Widened, want)
			}
			if err := protections.applyReplaced(v); err != nil {
				return fmt.Errorf("%w: %v", ErrContradictoryLog, err)
			}

		case ProtectionEnded:
			// An ending that was owed has had its levels checked already,
			// against the protection as it stood before the legs that same
			// fact cancelled were taken off it.
			current, ok := protections.levelsFor(v.Ref)
			if ok && !owedThis && (v.StopPrice != current.stopPrice || v.TargetPrice != current.targetPrice) {
				return fmt.Errorf("%w: an ending records levels %d/%d, the events before it say %d/%d",
					ErrContradictoryLog, v.StopPrice, v.TargetPrice, current.stopPrice, current.targetPrice)
			}
			if err := protections.applyEnded(v); err != nil {
				return fmt.Errorf("%w: %v", ErrContradictoryLog, err)
			}
		}
	}
	if err := protections.settled(); err != nil {
		return fmt.Errorf("%w: %v", ErrContradictoryLog, err)
	}
	return nil
}

// reduceWorking takes a fill off the order that produced it, if that order is
// waiting. A fill from an order that filled immediately matches nothing here,
// which is correct: it never rested.
func reduceWorking(working []market.Order, orderID string, qty market.Qty) ([]market.Order, error) {
	for n, o := range working {
		if o.ID != orderID {
			continue
		}
		remaining, err := market.AddQty(o.Qty, -qty)
		if err != nil {
			return nil, err
		}
		if remaining < 0 {
			return nil, fmt.Errorf("%w: order %s filled %d with %d working",
				ErrFabricated, orderID, qty, o.Qty)
		}
		if remaining == 0 {
			return append(working[:n], working[n+1:]...), nil
		}
		working[n].Qty = remaining
		return working, nil
	}
	return working, nil
}

func removeWorking(working []market.Order, orderID string) []market.Order {
	for n, o := range working {
		if o.ID == orderID {
			return append(working[:n], working[n+1:]...)
		}
	}
	return working
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

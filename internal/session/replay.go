package session

import (
	"errors"
	"fmt"
	"math"

	"praxis/internal/challenge"
	"praxis/internal/execution"
	"praxis/internal/market"
	"praxis/internal/portfolio"
)

// Errors reported when a journal cannot be reconstructed or does not agree
// with itself.
var (
	ErrNoSessionStarted   = errors.New("session: the log does not begin with a session start")

	// ErrIncoherentState reports a replayed state that is two claims at once
	// about the trading session: open under no name, or named with none open.
	//
	// Replay never builds one — the two move together, in one place each — but
	// ReplayedState is an exported struct of exported fields, so one can be
	// composed. A session resumed from it is past every door the constructor
	// holds: it reports open with nothing to name, and EndTradingSession then
	// records a boundary whose identifier is empty, which is a valid command
	// the record has no way to write and which kills the session at commit.
	// That is exactly the failure the identifier rule exists to prevent,
	// reachable again through a type rather than through a name.
	ErrIncoherentState = errors.New("session: the replayed state disagrees with itself about the open trading session")
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
	case ObservationPresented:
		return KindObservationPresented
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

	// LastObserved is the journal position of the observation now on the
	// screen, and Presented the one an interface confirmed showing. A resumed
	// session carries both: reconstruction is faithful or it is nothing, and a
	// new segment stops being confirmed on its own, because a confirmation
	// names the segment it was made in.
	LastObserved uint64
	Presented    PresentationID

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

	// clock is the chronology the journal reached. It is carried whole rather
	// than as two numbers, because a resumed session must refuse exactly what
	// the uninterrupted one would have refused: a recovery is not an
	// opportunity to launder a reading.
	clock interactionClock

	// Gestures is every human act the journal holds, and what each of them
	// commanded. A resumed session carries it so that a retry after a restart
	// is still recognised as the same decision rather than becoming a second
	// one — the authority is the journal, not a server's memory.
	Gestures []Gesture

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

	if err := pacingAgreesWithSubject(started.Config); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStructure, err)
	}

	state := &ReplayedState{Config: started.Config, Account: account, Challenge: eval}
	var (
		episodes         episodeProjection
		protections      protectionProjection
		gestures         gestureIndex
		lastTime         market.LogicalTime
		clock            = interactionClock{subjectID: started.Config.SubjectID, pacing: started.Config.Pacing}
		pendingChanges   []portfolio.PositionEvent
		pendingDecisions []challenge.Event

		// fillOrderID is the order whose fill the position changes now being
		// read belong to. A change does not name it, and protection binds to
		// the decision that opened the exposure, not to the exposure alone.
		fillOrderID string

		// offered says the last observation was given to everything waiting for
		// one, which is the condition under which what survived it has
		// something to answer for.
		offered bool

		// submitted is every order the trader has submitted that has not yet
		// reached an end, with what is left of it. The map answers membership
		// and quantity for one identifier at a time; the slice is the order
		// they were submitted in, so that what the check reports never depends
		// on iteration order.
		submitted   = map[string]market.Order{}
		outstanding []string
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

		if err := clock.Check(e); err != nil {
			return nil, fmt.Errorf("%w: event %d: %v", ErrStructure, n, err)
		}
		clock.Apply(e)
		if act, ok := gestureOf(e); ok {
			if err := gestures.claim(act); err != nil {
				return nil, fmt.Errorf("%w: event %d: %v", ErrFabricated, n, err)
			}
		}

		if _, ok := e.(PositionChanged); !ok && len(pendingChanges) > 0 && !owedConsequenceStandsHere(e, &protections) {
			return nil, fmt.Errorf("%w: event %d follows a fill whose %d changes were not recorded",
				ErrFabricated, n, len(pendingChanges))
		}
		if _, ok := e.(ChallengeDecision); !ok && len(pendingDecisions) > 0 {
			return nil, fmt.Errorf("%w: event %d follows %d unrecorded challenge decisions",
				ErrFabricated, n, len(pendingDecisions))
		}
		owedThis, err := protections.requireOwed(e)
		if err != nil {
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
			// The fill is re-executed against the book it met, before that
			// book is reduced by it.
			if err := proveFill(submitted, &protections, state, started.Config.Instrument, v.Fill); err != nil {
				return nil, fmt.Errorf("%w: event %d: %v", ErrFabricated, n, err)
			}
			if o, ok := submitted[v.Fill.OrderID]; ok {
				if o.Qty, err = market.AddQty(o.Qty, -v.Fill.Qty); err != nil {
					return nil, err
				}
				submitted[v.Fill.OrderID] = o
			}
			// The book this observation showed is finite, and this fill just
			// took part of it. What follows must meet what is left.
			if state.LastQuote, err = consumeBook(state.LastQuote, []market.Fill{v.Fill}); err != nil {
				return nil, err
			}
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
			// The projections run on changes already proved against what
			// applying the fill produced, so what they derive rests on facts
			// rather than on the log's word for them.
			//
			// What is left of this fill's effect says whether a close is a
			// reversal, which is the one thing the ending of a protection
			// turns on and the one thing a reader without the fills — Verify —
			// cannot see.
			flip := v.Change.Kind == portfolio.PositionClosed && len(pendingChanges) > 0
			if err := foldPositionChange(&episodes, &protections, v.Sequence, fillOrderID,
				v.Change, flip, true, func(owed owedEvent) error {
					protections.owe(owed)
					return nil
				}); err != nil {
				return nil, err
			}

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
			// Reconstruction is faithful or it is not reconstruction: a live
			// session drops the name here, so a replayed one does too.
			state.CurrentSessionID = ""

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

		case ObservationPresented:
			state.Presented = PresentationID{
				Segment: v.Presented.Segment, ObservedSequence: v.ObservedSequence,
			}

		case MarketObserved:
			// Before this observation replaces the last one, everything that
			// survived the last one has to account for surviving it.
			if offered {
				if err := proveNothingSurvivedFillable(state, &protections, started.Config.Instrument); err != nil {
					return nil, fmt.Errorf("%w: event %d: %v", ErrFabricated, n, err)
				}
			}
			state.LastQuote, state.HasQuote, state.ObservedThisSession = v.Quote, true, true
			state.LastObserved = v.Sequence
			// A session that is not open, or an evaluation that has ended, is
			// offered nothing — so nothing that waited through it owes an
			// explanation. This is the same gate the live session applies.
			offered = state.SessionOpen && eval.State() != challenge.StateFailed &&
				eval.State() != challenge.StatePassed

		case OrderRested:
			resting := v.Order
			resting.Qty = v.RestingQty
			state.Working = append(state.Working, resting)

		case OrderCancelled:
			state.Working = removeWorking(state.Working, v.OrderID)
			// A cancellation nothing owed is one the book itself has to
			// explain, and for a protective leg the book still can.
			if !owedThis {
				if err := proveLegCancellation(&protections, state.LastQuote, started.Config.Instrument, v); err != nil {
					return nil, fmt.Errorf("%w: event %d: %v", ErrFabricated, n, err)
				}
				if err := proveOrderCancellation(submitted, state.LastQuote, v); err != nil {
					return nil, fmt.Errorf("%w: event %d: %v", ErrFabricated, n, err)
				}
			}
			delete(submitted, v.OrderID)
			protections.entryGone(v.OrderID)
			protections.legCancelled(v.OrderID)

		case OrderSubmitted:
			submitted[v.Order.ID] = v.Order
			outstanding = append(outstanding, v.Order.ID)
			if state.OrdersThisSession, err = addOrders(state.OrdersThisSession, 1); err != nil {
				return nil, err
			}
			if err := protections.claim(v.Order.ID); err != nil {
				return nil, fmt.Errorf("%w: event %d: %v", ErrFabricated, n, err)
			}

		case ProtectionPlaced:
			// An entry and the levels planned with it are one act, so the
			// placement belongs to the submission it follows.
			gestures.attachProtection(v.StopPrice, v.TargetPrice)
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
	if offered {
		if err := proveNothingSurvivedFillable(state, &protections, started.Config.Instrument); err != nil {
			return nil, fmt.Errorf("%w: the log ends and %v", ErrFabricated, err)
		}
	}
	if err := proveEveryOrderEnded(submitted, outstanding, state.Working); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFabricated, err)
	}

	state.clock = clock
	state.PlannedProtections = protections.snapshot()
	state.ActiveProtections = protections.activeSnapshot()
	state.UsedOrderIDs = protections.usedIdentifiers()
	state.Gestures = gestures.snapshot()
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

// proveLegCancellation re-executes a protective leg against the book as it
// stood and refuses a cancellation the market could not have produced.
//
// A stop that reaches its level with nothing behind it leaves no position
// change, so for a while this was believed. It never had to be: Replay holds
// the observation, the leg's side, price and quantity, and what earlier fills
// took out of the book, and ConservativeExecution is a pure function of those.
// StopTriggered exists precisely to separate a level never reached from a level
// reached with no liquidity, which is the difference a forged cancellation
// depends on going unnoticed.
//
// The gap it closes was real: a journal could say a stop was withdrawn as
// unfillable while the quote that was supposed to have triggered it never came
// near the level, and nothing would have contradicted it.
//
// It applies only to a cancellation nothing owed, and only to a protective leg.
// A leg's remainder cancelled after a partial fill is already owed and proved
// from the position change; a trader's own order is still believed, because the
// book at the moment its remainder was cancelled is not the book its fills met.
func proveLegCancellation(protections *protectionProjection, book market.Quote, instrument market.Instrument, cancelled OrderCancelled) error {
	active, which, ok := protections.legNamed(cancelled.OrderID)
	if !ok {
		return nil
	}
	order, ok := protectiveOrder(active, which, instrument)
	if !ok {
		return nil
	}
	result, err := execution.ConservativeExecution{}.ExecuteOnQuote(order, book)
	if err != nil {
		return err
	}
	if !result.StopTriggered {
		return fmt.Errorf("%s was cancelled as unfillable, and %d/%d never reached %d",
			cancelled.OrderID, book.Bid, book.Ask, order.StopPrice)
	}
	if len(result.Fills) > 0 {
		return fmt.Errorf("%s was cancelled whole, and %d/%d still showed %d/%d for it",
			cancelled.OrderID, book.Bid, book.Ask, book.BidSize, book.AskSize)
	}
	if cancelled.RemainingQty != order.Qty {
		return fmt.Errorf("%s is cancelled with %d left, the protection covered %d",
			cancelled.OrderID, cancelled.RemainingQty, order.Qty)
	}
	if cancelled.Reason != CancelledUnfillableRemainder {
		return fmt.Errorf("%s is cancelled for %v; a leg that reached its level with nothing behind it ends as %v",
			cancelled.OrderID, cancelled.Reason, CancelledUnfillableRemainder)
	}
	return nil
}

// proveOrderCancellation refuses a remainder the book could still have filled,
// and an order that could not have had an unfillable remainder at all.
//
// Only two things produce one: a market order, which does not rest because
// resting it would invent a price the trader never named, and a stop that has
// triggered and therefore cannot go back to waiting. A limit rests, always.
//
// The reason matters more than it looks. Relabelling "the trader withdrew their
// stop" as "the system cancelled a remainder the book could not fill" launders
// a discretionary decision into a mechanical event — and that decision is the
// numerator of one of the hypotheses this whole log exists to measure.
func proveOrderCancellation(submitted map[string]market.Order, book market.Quote, cancelled OrderCancelled) error {
	if cancelled.Reason != CancelledUnfillableRemainder {
		return nil
	}
	order, ok := submitted[cancelled.OrderID]
	if !ok {
		return nil
	}
	if order.Type == market.OrderTypeLimit {
		return fmt.Errorf("%s is a limit order, and a limit rests rather than leaving an unfillable remainder",
			cancelled.OrderID)
	}
	if cancelled.RemainingQty != order.Qty {
		return fmt.Errorf("%s is cancelled with %d left, its fills leave %d",
			cancelled.OrderID, cancelled.RemainingQty, order.Qty)
	}
	result, err := execution.ConservativeExecution{}.ExecuteOnQuote(order, book)
	if err != nil {
		return err
	}
	if order.Type == market.OrderTypeStop && !result.StopTriggered {
		return fmt.Errorf("%s is a stop cancelled as unfillable, and %d/%d never reached %d",
			cancelled.OrderID, book.Bid, book.Ask, order.StopPrice)
	}
	if len(result.Fills) > 0 {
		return fmt.Errorf("%s was cancelled as unfillable, and %d/%d still showed %d/%d for it",
			cancelled.OrderID, book.Bid, book.Ask, book.BidSize, book.AskSize)
	}
	return nil
}

// proveEveryOrderEnded refuses a journal in which an order was submitted and
// then simply stopped being mentioned.
//
// Every order reaches one of three ends: it fills, it is cancelled, or it is
// still waiting when the log stops. An order that reaches none of them is
// invisible to every other check — it never enters the working set, so nothing
// asks it to account for surviving an observation, and it moved no money, so no
// valuation disagrees. A marketable limit that the log quietly forgets is a
// decision the trader made and the record does not contain.
func proveEveryOrderEnded(submitted map[string]market.Order, outstanding []string, working []market.Order) error {
	waiting := map[string]bool{}
	for _, o := range working {
		waiting[o.ID] = true
	}
	for _, id := range outstanding {
		order, live := submitted[id]
		if !live || order.Qty == 0 || waiting[id] {
			continue
		}
		return fmt.Errorf("%s was submitted, %d of it never filled, and the log neither rests nor cancels it",
			id, order.Qty)
	}
	return nil
}

// proveFill refuses a fill the book could not have given, at a price it could
// not have given, or from an order nobody submitted.
//
// The price is the gap this closes, and it is the plainest violation of
// "execution lies in favour of the market, never the trader" the log could
// hold: a market buy recorded ten ticks below the ask is ten ticks of free
// improvement, and the account is perfectly consistent with it afterwards
// because the fill is what fed the account. A valuation compares the account
// with itself and finds no quarrel.
//
// Quantity is recomputable for the same reason the book is consumed: a fill is
// exactly what the policy produced against the book at that moment, and every
// earlier fill has already been taken out of it. What this cannot see is which
// of two competing orders was offered the scarce depth first — the journal's
// own ordering decides that, and re-running the whole resolution is what would
// settle it.
func proveFill(submitted map[string]market.Order, protections *protectionProjection,
	state *ReplayedState, instrument market.Instrument, fill market.Fill) error {

	order, known := submitted[fill.OrderID]
	if !known {
		active, which, isLeg := protections.legNamed(fill.OrderID)
		if !isLeg {
			return fmt.Errorf("%s produced a fill and nothing submitted it", fill.OrderID)
		}
		if order, known = protectiveOrder(active, which, instrument); !known {
			return fmt.Errorf("%s produced a fill and the level it names is not set", fill.OrderID)
		}
	}

	result, err := execution.ConservativeExecution{}.ExecuteOnQuote(order, state.LastQuote)
	if err != nil {
		return err
	}
	if len(result.Fills) == 0 {
		return fmt.Errorf("%s filled %d at %d, and %d/%d showing %d/%d would have given it nothing",
			fill.OrderID, fill.Qty, fill.Price,
			state.LastQuote.Bid, state.LastQuote.Ask, state.LastQuote.BidSize, state.LastQuote.AskSize)
	}
	want := result.Fills[0]
	if fill.Price != want.Price {
		return fmt.Errorf("%s filled at %d, and %d/%d would have given it %d",
			fill.OrderID, fill.Price, state.LastQuote.Bid, state.LastQuote.Ask, want.Price)
	}
	if fill.Qty != want.Qty {
		return fmt.Errorf("%s filled %d, and %d/%d showing %d/%d would have given it %d",
			fill.OrderID, fill.Qty,
			state.LastQuote.Bid, state.LastQuote.Ask, state.LastQuote.BidSize, state.LastQuote.AskSize, want.Qty)
	}
	if fill.Side != want.Side || fill.Time != want.Time || fill.Instrument != want.Instrument {
		return fmt.Errorf("%s records %+v, the order and the book produce %+v", fill.OrderID, fill, want)
	}
	return nil
}

// proveNothingSurvivedFillable refuses a journal in which the market reached
// something that was waiting and the log says nothing happened to it.
//
// Everything that happened is proved against the aggregates. This is the other
// half: what did not happen. A journal could say a stop the market traded five
// ticks through is still working, and until this nothing contradicted it — the
// account is flat, so no valuation catches it, and the trader's loss simply
// never occurs. It is the one forgery direction that flatters a trader, which
// makes it the one worth closing first.
//
// It judges against the book **as it was left**, not as it arrived, so it makes
// no claim about how much would have filled and needs no second copy of the
// resolution order. Everything that actually filled has already been consumed;
// what remains is exactly what the survivors were offered. A limit that could
// not fill because an order ahead of it took the depth is therefore accepted,
// and a stop the price reached is not, because triggering owes nothing to
// liquidity.
func proveNothingSurvivedFillable(state *ReplayedState, protections *protectionProjection, instrument market.Instrument) error {
	if !state.HasQuote {
		return nil
	}
	policy := execution.ConservativeExecution{}

	for _, waiting := range state.Working {
		result, err := policy.ExecuteOnQuote(waiting, state.LastQuote)
		if err != nil {
			return err
		}
		if result.StopTriggered {
			return fmt.Errorf("%s is still working, and %d/%d reached its stop at %d",
				waiting.ID, state.LastQuote.Bid, state.LastQuote.Ask, waiting.StopPrice)
		}
		if len(result.Fills) > 0 {
			return fmt.Errorf("%s is still working, and %d/%d still showed %d/%d for it",
				waiting.ID, state.LastQuote.Bid, state.LastQuote.Ask,
				state.LastQuote.BidSize, state.LastQuote.AskSize)
		}
	}

	for _, id := range protections.activeEpisodeIDs() {
		active, ok := protections.activeFor(id)
		if !ok {
			continue
		}
		for _, which := range []legKind{legStop, legTarget} {
			leg, ok := protectiveOrder(active, which, instrument)
			if !ok {
				continue
			}
			result, err := policy.ExecuteOnQuote(leg, state.LastQuote)
			if err != nil {
				return err
			}
			if result.StopTriggered {
				return fmt.Errorf("%s still protects episode %d, and %d/%d reached its level at %d",
					leg.ID, id, state.LastQuote.Bid, state.LastQuote.Ask, leg.StopPrice)
			}
			if len(result.Fills) > 0 {
				return fmt.Errorf("%s still protects episode %d, and %d/%d still showed %d/%d for it",
					leg.ID, id, state.LastQuote.Bid, state.LastQuote.Ask,
					state.LastQuote.BidSize, state.LastQuote.AskSize)
			}
		}
	}
	return nil
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
	// Whether a trading session is open and which one it is are one fact in
	// two fields. A session is only ever as coherent as the state it was
	// resumed from, so this is where that fact is held together.
	if state.SessionOpen != (state.CurrentSessionID != "") {
		return nil, fmt.Errorf("%w: open=%v, named %q",
			ErrIncoherentState, state.SessionOpen, state.CurrentSessionID)
	}
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
		lastObserved:        state.LastObserved,
		presented:           state.Presented,
		clock:               state.clock,
		ordersThisSession:   state.OrdersThisSession,
		consecutiveLosses:   state.ConsecutiveLosses,
		episodes:            state.episodes,
		sessionRealisedCts:  state.SessionRealisedCts,
		working:             restoreWorking(state.Working),
		committer:           committer,
	}
	resumed.protections.restore(state.PlannedProtections, state.ActiveProtections,
		state.Config.Instrument.Symbol, state.UsedOrderIDs)
	resumed.gestures.restore(state.Gestures)
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
		gestures    gestureIndex

		// sides remembers which way an entry would open, which is what makes a
		// stop's move away from it a widening.
		sides = map[string]bool{}

		// fillOrderID is the order whose fill the position changes now being
		// read belong to.
		fillOrderID string

		// clock is the same rule Replay applies, asked by the other reader.
		clock interactionClock
	)

	if len(events) > 0 {
		if started, ok := events[0].(SessionStarted); ok {
			// The run's own claim about whether anybody was there has to hold
			// together before it is used to judge anything else. Replay asks
			// this too; Verify asking it as well is what keeps Verify as
			// strong on its own as it was before the clock read pacing.
			if err := pacingAgreesWithSubject(started.Config); err != nil {
				return fmt.Errorf("%w: %v", ErrContradictoryLog, err)
			}
			clock.subjectID, clock.pacing = started.Config.SubjectID, started.Config.Pacing
		}
	}

	for _, e := range events {
		owedThis, owedErr := protections.requireOwed(e)
		if owedErr != nil {
			return fmt.Errorf("%w: %v", ErrContradictoryLog, owedErr)
		}
		if err := clock.Check(e); err != nil {
			return fmt.Errorf("%w: %v", ErrContradictoryLog, err)
		}
		clock.Apply(e)
		if act, ok := gestureOf(e); ok {
			if err := gestures.claim(act); err != nil {
				return fmt.Errorf("%w: %v", ErrContradictoryLog, err)
			}
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
			// Verify has the events and not the fills, so it cannot tell a
			// reversal from an exit — the one thing an ending's reason turns
			// on. It demands the same consequences with that reason unchecked,
			// which is a prefix of what Replay demands.
			if err := foldPositionChange(&episodes, &protections, v.Sequence, fillOrderID,
				v.Change, false, false, func(owed owedEvent) error {
					protections.owe(owed)
					return nil
				}); err != nil {
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
			gestures.attachProtection(v.StopPrice, v.TargetPrice)
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

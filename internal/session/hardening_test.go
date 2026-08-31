package session_test

import (
	"errors"
	"math"
	"reflect"
	"testing"

	"praxis/internal/challenge"
	"praxis/internal/market"
	"praxis/internal/portfolio"
	"praxis/internal/session"
)

// Scenario: a session whose valuation is missing cannot validate a decision
// against the previous session's figures
//
// The account is deliberately untouched in the first session, so the stale
// figures are numerically identical to the ones the decision recorded. Only
// resetting the valuation at the boundary catches this; comparing the numbers
// cannot, because they agree.
func TestVerifyRejectsADecisionWithNoValuationInItsSession(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	if err := s.EndTradingSession(3_000); err != nil {
		t.Fatalf("EndTradingSession: %v", err)
	}
	mustOpen(t, s, 4_000, "d2")
	mustObserve(t, s, quote(5_000, 20_000, 20_001))
	mustSubmit(t, s, order("o-1", market.SideBuy, 1))

	events := s.Events()
	if err := session.Verify(events); err != nil {
		t.Fatalf("the intact log does not verify: %v", err)
	}

	var pruned []session.Event
	inSecond := false
	for _, e := range events {
		if o, ok := e.(session.SessionOpened); ok && o.SessionID == "d2" {
			inSecond = true
		}
		if _, ok := e.(session.AccountValued); ok && inSecond {
			continue
		}
		pruned = append(pruned, e)
	}

	if err := session.Verify(pruned); !errors.Is(err, session.ErrContradictoryLog) {
		t.Fatalf("error: got %v, want %v", err, session.ErrContradictoryLog)
	}
}

// Scenario: an out-of-order boundary leaves nothing behind
//
//	Given a session already past a logical time
//	When a trading session is opened before it
//	Then the evaluation must not have accepted the boundary. If it had, the
//	  challenge would be in one session and the log in another, with nothing
//	  able to reconcile them.
func TestALateBoundaryDoesNotMoveTheEvaluation(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 5_000, "d1")
	if err := s.EndTradingSession(6_000); err != nil {
		t.Fatalf("EndTradingSession: %v", err)
	}

	before := snapshot(t, s)

	if err := s.OpenTradingSession(4_000, "d2"); !errors.Is(err, session.ErrOutOfOrder) {
		t.Fatalf("error: got %v, want %v", err, session.ErrOutOfOrder)
	}

	if got := snapshot(t, s); !reflect.DeepEqual(got, before) {
		t.Fatalf("a rejected boundary changed the session\n got: %+v\nwas: %+v", got, before)
	}
	if s.Challenge().SessionID() != "d1" {
		t.Fatalf("the evaluation moved to %v", s.Challenge().SessionID())
	}
}

type sessionSnapshot struct {
	events      int
	challenge   challengeState
	realisedCts market.Cents
	feesCts     market.Cents
	positions   []portfolio.Position
	openID      challenge.SessionID
}

func snapshot(t *testing.T, s *session.Session) sessionSnapshot {
	t.Helper()
	return sessionSnapshot{
		events: s.JournalLen(), challenge: describe(s.Challenge()),
		realisedCts: s.Account().RealisedCts(), feesCts: s.Account().FeesCts(),
		positions: s.Account().Positions(), openID: s.OpenSessionID(),
	}
}

// Scenario: an order the account refuses leaves no trace at all
//
//	Given a fill whose arithmetic cannot be represented
//	When the order is submitted
//	Then no order, no fill and no position change is recorded, no counter
//	  moves, the account is untouched, and the log still replays.
func TestAnOrderTheAccountRefusesLeavesNoTrace(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	huge := market.Quote{
		Instrument: mnq, Time: 3_000,
		Bid: 20_000, Ask: 20_001,
		BidSize: math.MaxInt64, AskSize: math.MaxInt64,
	}
	mustObserve(t, s, huge)

	before := snapshot(t, s)

	err := s.SubmitOrder(order("o-1", market.SideBuy, math.MaxInt64))
	if !errors.Is(err, market.ErrOverflow) {
		t.Fatalf("error: got %v, want %v", err, market.ErrOverflow)
	}

	if got := snapshot(t, s); !reflect.DeepEqual(got, before) {
		t.Fatalf("a refused order changed the session\n got: %+v\nwas: %+v", got, before)
	}
	for _, e := range s.Events() {
		switch e.(type) {
		case session.OrderSubmitted, session.FillProduced, session.PositionChanged:
			t.Fatalf("a refused order was recorded as %v", e.Header().Kind)
		}
	}
	if err := session.Verify(s.Events()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if _, err := session.Replay(s.Events()); err != nil {
		t.Fatalf("Replay: %v", err)
	}
}

// The journal is not reachable from outside, and the copy handed out cannot
// reach back into it.
func TestTheJournalCannotBeWrittenFromOutside(t *testing.T) {
	s := script(t)
	events := s.Events()
	before := s.JournalLen()

	events[0] = session.SessionEnded{}
	events = append(events, session.SessionEnded{})

	if s.JournalLen() != before {
		t.Fatalf("the journal grew to %d", s.JournalLen())
	}
	if _, ok := s.Events()[0].(session.SessionStarted); !ok {
		t.Fatalf("the journal's first event was overwritten from outside")
	}
}

// Scenario: two starting balances that disagree are rejected before anything
// is recorded
func TestNewRejectsTwoDisagreeingStartingBalances(t *testing.T) {
	cfg := config()
	cfg.Rules.StartingBalanceCts = cfg.StartingBalanceCts + 1

	if _, err := session.New(cfg, 1_000); !errors.Is(err, session.ErrInconsistentConfig) {
		t.Fatalf("error: got %v, want %v", err, session.ErrInconsistentConfig)
	}
}

// A decision's own position in the log is not the position of the input that
// caused it.
func TestAChallengeDecisionSeparatesItsPositionFromItsCause(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")

	var seen int
	for _, e := range s.Events() {
		d, ok := e.(session.ChallengeDecision)
		if !ok {
			continue
		}
		seen++
		if d.CausedBySequence >= d.Sequence {
			t.Fatalf("decision at %d claims to be caused by %d", d.Sequence, d.CausedBySequence)
		}
		if d.Decision.Kind == 0 {
			t.Fatalf("decision at %d carries no kind", d.Sequence)
		}
	}
	if seen == 0 {
		t.Fatal("no challenge decision was recorded")
	}
}

// Verify's own arithmetic is checked. A log whose realised amounts cannot be
// summed is rejected, rather than accepted because the verifier wrapped.
//
// The log is built by hand: a real session cannot produce these amounts, which
// is the point — a verifier defends against a log it did not produce.
func TestVerifyRejectsALogItCannotAddUp(t *testing.T) {
	closing := func(seq uint64, realisedCts market.Cents) session.PositionChanged {
		return session.PositionChanged{
			Envelope: session.Envelope{Time: 3_000, Sequence: seq, Kind: session.KindPositionChanged},
			Change: portfolio.PositionEvent{
				Kind: portfolio.PositionClosed, Instrument: mnq,
				Side: market.SideSell, Qty: 1, Price: 20_000, RealisedCts: realisedCts,
			},
		}
	}

	events := []session.Event{
		session.SessionStarted{Envelope: session.Envelope{Time: 1_000, Sequence: 1, Kind: session.KindSessionStarted}, Config: config()},
		session.SessionOpened{Envelope: session.Envelope{Time: 2_000, Sequence: 2, Kind: session.KindSessionOpened}, SessionID: "d1"},
		session.AccountValued{Envelope: session.Envelope{Time: 2_000, Sequence: 3, Kind: session.KindAccountValued}, SessionID: "d1"},
		closing(4, math.MinInt64),
		closing(5, -1),
	}

	if err := session.Verify(events); !errors.Is(err, market.ErrOverflow) {
		t.Fatalf("error: got %v, want %v", err, market.ErrOverflow)
	}
}

// Scenario: a session resumed from its journal carries on identically
//
//	Given a run cut in half and rebuilt from its events
//	When the remaining commands are applied to the resumed session
//	Then the whole stream is identical to one that was never interrupted.
func TestAResumedSessionProducesTheSameStream(t *testing.T) {
	uninterrupted := script(t).Events()

	// The same script, stopped after the first trading session.
	cut := newSession(t)
	mustOpen(t, cut, 2_000, "d1")
	mustObserve(t, cut, quote(3_000, 20_000, 20_001))
	mustSubmit(t, cut, order("o-1", market.SideBuy, 3))
	mustObserve(t, cut, quote(4_000, 20_010, 20_011))
	mustSubmit(t, cut, order("o-2", market.SideSell, 1))
	mustObserve(t, cut, quote(5_000, 19_990, 19_991))
	mustSubmit(t, cut, order("o-3", market.SideSell, 2))
	mustSubmit(t, cut, order("o-4", market.SideBuy, 2))
	if err := cut.EndTradingSession(6_000); err != nil {
		t.Fatalf("EndTradingSession: %v", err)
	}

	state, err := session.Replay(cut.Events())
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	resumed, err := session.Resume(state)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	mustOpen(t, resumed, 7_000, "d2")
	mustObserve(t, resumed, quote(8_000, 19_995, 19_996))
	mustSubmit(t, resumed, order("o-5", market.SideSell, 2))
	mustObserve(t, resumed, quote(9_000, 19_985, 19_986))
	if err := resumed.EndTradingSession(10_000); err != nil {
		t.Fatalf("EndTradingSession: %v", err)
	}

	if !reflect.DeepEqual(resumed.Events(), uninterrupted) {
		t.Fatalf("a resumed session diverged from an uninterrupted one")
	}
	if err := session.Verify(resumed.Events()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// A session resumed mid-trading-session keeps the counters a decision is
// measured against.
func TestResumeCarriesTheBehaviouralCounters(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, quote(3_000, 20_000, 20_001))
	mustSubmit(t, s, order("o-1", market.SideBuy, 3))
	mustObserve(t, s, quote(4_000, 19_900, 19_901))
	mustSubmit(t, s, order("o-2", market.SideSell, 3)) // a losing close

	state, err := session.Replay(s.Events())
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if state.ConsecutiveLosses != 1 || state.OrdersThisSession != 2 || state.SessionRealisedCts >= 0 {
		t.Fatalf("state: got %+v, want one loss after two orders", state)
	}
	if !state.SessionOpen || state.CurrentSessionID != "d1" || !state.ObservedThisSession {
		t.Fatalf("state: got %+v, want the trading session still open and observed", state)
	}

	resumed, err := session.Resume(state)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	mustSubmit(t, resumed, order("o-3", market.SideBuy, 1))

	ctx := orderContext(t, resumed, "o-3")
	if ctx.OrdersSubmittedThisSession != 2 || ctx.ConsecutiveLosses != 1 {
		t.Fatalf("context: got %+v, want the counters carried across the resume", ctx)
	}
	if err := session.Verify(resumed.Events()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

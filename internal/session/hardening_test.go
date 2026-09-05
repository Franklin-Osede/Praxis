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

// Scenario: a command that fails after recording stops the session
//
//	Given a fill whose arithmetic the account cannot represent
//	When the order is submitted
//	Then the account is untouched, and the session is finished rather than
//	  usable — because events are already in its journal that will never be
//	  committed, and continuing would build the next batch on a position the
//	  store has never seen.
//
// This branch was unreachable and marked as such until working orders arrived.
// An observation now records itself before offering the quote to a waiting
// stop, and an order records itself before its fills are applied, so a refusal
// from the account happens after the log has spoken.
func TestACommandThatFailsAfterRecordingStopsTheSession(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	huge := market.Quote{
		Instrument: mnq, Time: 3_000,
		Bid: 20_000, Ask: 20_001,
		BidSize: math.MaxInt64, AskSize: math.MaxInt64,
	}
	mustObserve(t, s, huge)

	positionsBefore := s.Account().Positions()
	realisedBefore, feesBefore := s.Account().RealisedCts(), s.Account().FeesCts()

	err := s.SubmitOrder(order("o-1", market.SideBuy, math.MaxInt64))
	if !errors.Is(err, session.ErrSessionNeedsRecovery) {
		t.Fatalf("error: got %v, want %v", err, session.ErrSessionNeedsRecovery)
	}
	if !errors.Is(err, market.ErrOverflow) {
		t.Fatalf("the cause is not reported: %v", err)
	}
	if s.NeedsRecovery() == nil {
		t.Fatal("the session does not report that it needs recovery")
	}

	// The money is untouched: ApplyFill is atomic and refused before changing
	// anything.
	if !reflect.DeepEqual(s.Account().Positions(), positionsBefore) ||
		s.Account().RealisedCts() != realisedBefore || s.Account().FeesCts() != feesBefore {
		t.Fatal("a refused fill changed the account")
	}

	// And the session is finished, whatever the store would now accept.
	if err := s.Observe(quote(4_000, 20_000, 20_001), 1); !errors.Is(err, session.ErrSessionNeedsRecovery) {
		t.Fatalf("a stopped session accepted a command: %v", err)
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

	if _, err := session.New(cfg, 1_000, nil); !errors.Is(err, session.ErrInconsistentConfig) {
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
				Kind: portfolio.PositionReduced, Instrument: mnq,
				Side: market.SideSell, Qty: 1, Price: 20_000, RealisedCts: realisedCts,
			},
		}
	}

	opened := session.PositionChanged{
		Envelope: session.Envelope{Time: 3_000, Sequence: 4, Kind: session.KindPositionChanged},
		Change: portfolio.PositionEvent{
			Kind: portfolio.PositionOpened, Instrument: mnq,
			Side: market.SideBuy, Qty: 3, Price: 20_000,
		},
	}
	// Reductions rather than closes, so the log describes a coherent episode
	// and the only thing wrong with it is that its realised amounts cannot be
	// added up.
	events := []session.Event{
		session.SessionStarted{Envelope: session.Envelope{Time: 1_000, Sequence: 1, Kind: session.KindSessionStarted}, Config: config()},
		session.SessionOpened{Envelope: session.Envelope{Time: 2_000, Sequence: 2, Kind: session.KindSessionOpened}, SessionID: "d1"},
		session.AccountValued{Envelope: session.Envelope{Time: 2_000, Sequence: 3, Kind: session.KindAccountValued}, SessionID: "d1"},
		opened,
		closing(5, math.MinInt64),
		closing(6, -1),
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
	resumed, err := session.Resume(state, nil)
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

	resumed, err := session.Resume(state, nil)
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

// failOnce is a committer that refuses exactly one batch and then works. It
// exists so that a stopped session can be shown to refuse a command the store
// would have accepted, which a committer that stayed broken could not prove.
type failOnce struct {
	failAt    int
	committed int
	batches   [][]session.Event
}

func (c *failOnce) Commit(events []session.Event) error {
	c.committed++
	if c.committed == c.failAt {
		return errors.New("injected commit failure")
	}
	c.batches = append(c.batches, events)
	return nil
}

// Scenario: a session that failed to commit is finished, even when the store
// has recovered
//
//	Given a store that failed one batch and would accept the next
//	When another command is issued
//	Then the session still refuses. Nothing inside it can know what reached
//	  the disk, so carrying on would be guessing; it is replaced by one
//	  rebuilt from the confirmed batches, never healed.
func TestAStoppedSessionRefusesEvenWhenTheStoreRecovers(t *testing.T) {
	committer := &failOnce{failAt: 2}

	s, err := session.New(config(), 1_000, committer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if committer.committed != 1 {
		t.Fatalf("starting a session committed %d batches, want 1", committer.committed)
	}

	err = s.OpenTradingSession(2_000, "d1")
	if !errors.Is(err, session.ErrSessionNeedsRecovery) {
		t.Fatalf("error: got %v, want %v", err, session.ErrSessionNeedsRecovery)
	}
	if s.NeedsRecovery() == nil {
		t.Fatal("the session does not report that it needs recovery")
	}

	before := committer.committed
	for _, attempt := range []struct {
		name string
		run  func() error
	}{
		{"opening a session", func() error { return s.OpenTradingSession(3_000, "d2") }},
		{"observing", func() error {
			return s.Observe(market.Quote{Instrument: mnq, Time: 3_000, Bid: 20_000, Ask: 20_001, BidSize: 1, AskSize: 1}, 1)
		}},
		{"ending a session", func() error { return s.EndTradingSession(4_000) }},
	} {
		if err := attempt.run(); !errors.Is(err, session.ErrSessionNeedsRecovery) {
			t.Fatalf("%s: got %v, want %v", attempt.name, err, session.ErrSessionNeedsRecovery)
		}
	}
	if committer.committed != before {
		t.Fatalf("a stopped session attempted %d further commits", committer.committed-before)
	}
}

// Scenario: one command is one batch
//
// Every event a command produced is committed together, and a command that
// produced none commits nothing.
func TestOneCommandCommitsExactlyOneBatch(t *testing.T) {
	committer := &failOnce{failAt: -1}

	s, err := session.New(config(), 1_000, committer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, quote(3_000, 20_000, 20_001))
	mustSubmit(t, s, order("o-1", market.SideBuy, 2))

	if len(committer.batches) != 4 {
		t.Fatalf("batches: got %d, want one per command", len(committer.batches))
	}

	// The batches, laid end to end, are the journal exactly once.
	var flattened []session.Event
	for _, b := range committer.batches {
		flattened = append(flattened, b...)
	}
	if !reflect.DeepEqual(flattened, s.Events()) {
		t.Fatal("the committed batches do not add up to the journal")
	}

	// A refused command commits nothing at all.
	before := len(committer.batches)
	if err := s.OpenTradingSession(4_000, "d2"); err == nil {
		t.Fatal("opening a second session was accepted")
	}
	if len(committer.batches) != before {
		t.Fatal("a refused command was committed")
	}
}

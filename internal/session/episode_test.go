package session_test

import (
	"testing"

	"praxis/internal/market"
	"praxis/internal/session"
)

func streakAt(t *testing.T, s *session.Session, orderID string) uint32 {
	t.Helper()
	return orderContext(t, s, orderID).ConsecutiveLosingTrades
}

// Scenario: a streak counts completed trades, not closing legs
//
//	Given one bad position scaled out of in two reductions
//	When the next order is submitted
//	Then the streak is one, not two: abandoning a position in pieces is one
//	  trade going wrong, and counting the pieces would report a run the
//	  trader never had.
func TestAStreakCountsTradesAndNotClosingLegs(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	mustSubmit(t, s, order("in", market.SideBuy, 4))

	// Out in two losing pieces.
	mustObserve(t, s, sized(4_000, 19_900, 19_901, 50))
	mustSubmit(t, s, order("out-a", market.SideSell, 2))
	mustSubmit(t, s, order("out-b", market.SideSell, 2))
	mustSubmit(t, s, order("next", market.SideBuy, 1))

	ctx := orderContext(t, s, "next")
	if ctx.ConsecutiveLosingTrades != 1 {
		t.Fatalf("losing trades: got %d, want 1", ctx.ConsecutiveLosingTrades)
	}
	if ctx.ConsecutiveLosses != 2 {
		t.Fatalf("losing closes: got %d, want 2 — both counters are facts", ctx.ConsecutiveLosses)
	}
}

// Scenario: an open trade is not a losing one
func TestAnOpenTradeIsNotCountedAsALoss(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	mustSubmit(t, s, order("in", market.SideBuy, 4))

	// Under water, and still open. Not so far under that the evaluation ends,
	// which would stop the session for a different reason than the one under
	// test.
	mustObserve(t, s, sized(4_000, 19_900, 19_901, 50))
	mustSubmit(t, s, order("add", market.SideBuy, 1))

	if got := streakAt(t, s, "add"); got != 0 {
		t.Fatalf("losing trades: got %d, want 0 — it has not ended", got)
	}
}

// Scenario: break-even includes commissions
//
//	Given a trade closed at exactly the price it opened at
//	When the streak is read afterwards
//	Then it counts as a loss, because the commission was real money and an
//	  episode that gave its gain back in fees was not economically flat.
func TestBreakEvenOnPriceIsALossAfterFees(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	// A locked book, so the round trip is flat on price and only fees move.
	mustObserve(t, s, market.Quote{Instrument: mnq, Time: 3_000, Bid: 20_000, Ask: 20_000, BidSize: 50, AskSize: 50})
	mustSubmit(t, s, order("in", market.SideBuy, 2))
	mustSubmit(t, s, order("out", market.SideSell, 2))
	mustSubmit(t, s, order("next", market.SideBuy, 1))

	if got := streakAt(t, s, "next"); got != 1 {
		t.Fatalf("losing trades: got %d, want 1 — the fees were real", got)
	}
}

// Scenario: a winning trade breaks the streak
func TestAWinBreaksTheStreak(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	mustSubmit(t, s, order("lose-in", market.SideBuy, 2))
	mustObserve(t, s, sized(4_000, 19_900, 19_901, 50))
	mustSubmit(t, s, order("lose-out", market.SideSell, 2))

	mustSubmit(t, s, order("win-in", market.SideBuy, 2))
	if got := streakAt(t, s, "win-in"); got != 1 {
		t.Fatalf("losing trades: got %d, want 1", got)
	}

	mustObserve(t, s, sized(5_000, 20_500, 20_501, 50))
	mustSubmit(t, s, order("win-out", market.SideSell, 2))
	mustSubmit(t, s, order("after", market.SideBuy, 1))

	if got := streakAt(t, s, "after"); got != 0 {
		t.Fatalf("losing trades: got %d, want the win to have broken the streak", got)
	}
}

// Scenario: the order that flips carries the streak from before the flip
//
//	Given a losing long and an order large enough to reverse it
//	When that order is submitted
//	Then its context carries the streak as it stood before the flip, because
//	  the decision was taken before the episode ended
//	And the next order sees the episode the flip closed.
//
// This is a fact about when a decision was taken, not about when its
// consequences were recorded, and confusing the two would attribute knowledge
// to the trader that they did not have.
func TestTheFlippingOrderCarriesTheEarlierStreak(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	mustSubmit(t, s, order("in", market.SideBuy, 2))

	// Sell five into a long of two: closes the long at a loss, opens a short.
	mustObserve(t, s, sized(4_000, 19_900, 19_901, 50))
	mustSubmit(t, s, order("flip", market.SideSell, 5))

	if got := streakAt(t, s, "flip"); got != 0 {
		t.Fatalf("the flipping order: got %d, want 0 — the episode had not ended when it was decided", got)
	}

	p, _ := s.Account().Position(mnq)
	if p.NetQty != -3 {
		t.Fatalf("position: got %d, want short three", p.NetQty)
	}

	mustSubmit(t, s, order("after", market.SideSell, 1))
	if got := streakAt(t, s, "after"); got != 1 {
		t.Fatalf("the next order: got %d, want it to see the episode the flip closed", got)
	}
}

// The streak survives a journal, and a resumed session continues it rather
// than starting again.
func TestTheStreakSurvivesReplayAndResume(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	mustSubmit(t, s, order("in", market.SideBuy, 2))
	mustObserve(t, s, sized(4_000, 19_900, 19_901, 50))
	mustSubmit(t, s, order("out", market.SideSell, 2))

	state, err := session.Replay(s.Events())
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if state.ConsecutiveLosingTrades != 1 {
		t.Fatalf("replayed streak: got %d, want 1", state.ConsecutiveLosingTrades)
	}
	if err := session.Verify(s.Events()); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	resumed, err := session.Resume(state, nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	mustSubmit(t, resumed, order("after", market.SideBuy, 1))
	if got := streakAt(t, resumed, "after"); got != 1 {
		t.Fatalf("a resumed session restarted the streak: got %d", got)
	}
}

// An episode open when a run stops must still be open when it resumes, or the
// close that ends it would arrive with nothing to end.
func TestAnOpenEpisodeSurvivesAResume(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	mustSubmit(t, s, order("in", market.SideBuy, 2))

	state, err := session.Replay(s.Events())
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	resumed, err := session.Resume(state, nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	mustObserve(t, resumed, sized(4_000, 19_900, 19_901, 50))
	mustSubmit(t, resumed, order("out", market.SideSell, 2))
	mustSubmit(t, resumed, order("after", market.SideBuy, 1))

	if got := streakAt(t, resumed, "after"); got != 1 {
		t.Fatalf("losing trades: got %d, want the resumed episode to have closed at a loss", got)
	}
}

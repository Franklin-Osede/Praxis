package session_test

import (
	"testing"

	"praxis/internal/market"
)

// Scenario: a working order and its protection cross a session boundary
//
//	Given a limit resting away from the market, with a protection planned
//	  against it, when its trading session ends and another opens
//	Then both are still there, and the first book of the new session fills the
//	  order and activates the plan.
//
// Orders outlive a boundary the way a position does. That is the behaviour and
// it now has a test, because until it did it was an accident: openTradingSession
// resets the counters, the observed flag and the evaluation's reference, and
// leaves s.working alone only because nobody wrote the line. A paragraph in the
// specification saying so would have documented an omission, and the next
// person to add `s.working = nil` there would have broken nothing red.
//
// It is deliberately not TimeInForce. One behaviour needs no abstraction, and
// if the pilots turn out to want day orders that is a typed field and a typed
// cancellation, arrived at on purpose — not a silent change of what a journal
// already means.
func TestAWorkingOrderAndItsProtectionCrossABoundary(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))

	// Far below the market, so it rests, and protected on both sides.
	entry, err := market.NewLimitOrder("o-1", mnq, market.SideBuy, 2, 19_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitOrderWithProtection(entry, 18_000, 19_500, decidedAt()); err != nil {
		t.Fatalf("SubmitOrderWithProtection: %v", err)
	}
	if len(s.WorkingOrders()) != 1 || len(s.PlannedProtections()) != 1 {
		t.Fatalf("in d1: working %d, planned %d, want one of each",
			len(s.WorkingOrders()), len(s.PlannedProtections()))
	}

	if err := s.EndTradingSession(4_000); err != nil {
		t.Fatalf("EndTradingSession: %v", err)
	}
	mustOpen(t, s, 5_000, "d2")

	working, planned := s.WorkingOrders(), s.PlannedProtections()
	if len(working) != 1 || len(planned) != 1 {
		t.Fatalf("in d2: working %d, planned %d, want both still standing",
			len(working), len(planned))
	}
	if working[0].ID != "o-1" || working[0].Qty != 2 || working[0].LimitPrice != 19_000 {
		t.Fatalf("the order changed across the boundary: %+v", working[0])
	}
	if planned[0].EntryOrderID != "o-1" || planned[0].StopPrice != 18_000 || planned[0].TargetPrice != 19_500 {
		t.Fatalf("the plan changed across the boundary: %+v", planned[0])
	}

	// And the new session's first book is offered to it, exactly as the old
	// session's would have been.
	mustObserve(t, s, sized(6_000, 18_900, 18_901, 50))

	if len(s.WorkingOrders()) != 0 {
		t.Fatalf("the order did not fill against the new session's book: %+v", s.WorkingOrders())
	}
	if len(s.PlannedProtections()) != 0 {
		t.Fatalf("the plan did not activate: %+v", s.PlannedProtections())
	}
	active := s.ActiveProtections()
	if len(active) != 1 || active[0].ProtectedQty != 2 {
		t.Fatalf("active protection: got %+v, want one covering 2", active)
	}
	if position, _ := s.Account().Position(mnq); position.NetQty != 2 {
		t.Fatalf("position: got %d, want 2", position.NetQty)
	}
	checked(t, s)
}

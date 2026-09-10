package session_test

import (
	"testing"

	"praxis/internal/market"
)

// Scenario: one act is looked up by name
//
//	Given a journal holding a human act
//	Then asking for it by its identifier returns what it commanded, and asking
//	  for one the journal does not hold says so.
//
// The loop answers a retry on every request that carries a gesture. Handing
// back a copy of every act to find one is what a command used to do with the
// whole journal, and it grows the same way.
func TestAnActIsFoundByItsName(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))

	act := decidedAt()
	if err := s.SubmitOrder(order("o-1", market.SideBuy, 2), act); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}

	found, ok := s.Gesture(act.GestureID)
	if !ok {
		t.Fatalf("the act %s is not in the register", act.GestureID)
	}
	if found.Order.ID != "o-1" || found.Order.Qty != 2 {
		t.Fatalf("the act commanded %+v, want the order it submitted", found.Order)
	}
	if found.Decided != act {
		t.Fatalf("the stamp changed: got %+v, want %+v", found.Decided, act)
	}

	if _, ok := s.Gesture("g-never-sent"); ok {
		t.Fatal("an act the journal does not hold was found")
	}
}

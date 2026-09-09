package session

import (
	"testing"
)

// Scenario: restore replaces a projection's state, all of it
//
//	Given a projection already holding something
//	When it is restored from a different journal's state
//	Then nothing of the first survives.
//
// Both are only ever called on a projection a Resume just made, so today the
// leak is unreachable. It is closed anyway because the cost is two lines and
// the failure is silent: a name that survived would refuse an order the second
// journal never spent, and a gesture that survived would answer a retry with
// what a different run committed. Clearing one collection and keeping another
// is a trap left lying about, and this repository takes those out rather than
// relying on the caller to keep not stepping in them.
func TestRestoreReplacesEverything(t *testing.T) {
	t.Run("a gesture index", func(t *testing.T) {
		var g gestureIndex
		first := Gesture{
			Kind:    GestureSubmitOrder,
			Decided: Decision{GestureID: "g-old", AtUTCNanos: 1, Segment: 1, ElapsedNanos: 1},
		}
		if err := g.claim(first); err != nil {
			t.Fatalf("claim: %v", err)
		}

		g.restore([]Gesture{{
			Kind:    GestureSubmitOrder,
			Decided: Decision{GestureID: "g-new", AtUTCNanos: 2, Segment: 1, ElapsedNanos: 2},
		}})

		if g.used("g-old") {
			t.Fatal("a gesture from the journal it was restored away from is still spent")
		}
		if _, ok := g.find("g-old"); ok {
			t.Fatal("a gesture from another run would answer a retry")
		}
		if !g.used("g-new") {
			t.Fatal("the restored gesture is not there")
		}
		if len(g.snapshot()) != 1 {
			t.Fatalf("acts: got %d, want only the restored one", len(g.snapshot()))
		}
	})

	t.Run("a protection projection", func(t *testing.T) {
		var p protectionProjection
		if err := p.claim("praxis:1:stop"); err != nil {
			t.Fatalf("claim: %v", err)
		}
		p.planned = append(p.planned, plannedProtection{entryOrderID: "o-old"})

		p.restore(
			[]PlannedProtection{{EntryOrderID: "o-new", StopPrice: 10, TargetPrice: 20}},
			nil, "MNQ", []string{"praxis:2:stop"},
		)

		if p.used("praxis:1:stop") {
			t.Fatal("a name from the journal it was restored away from is still spent")
		}
		if !p.used("praxis:2:stop") {
			t.Fatal("the restored name is not spent")
		}
		if got := p.snapshot(); len(got) != 1 || got[0].EntryOrderID != "o-new" {
			t.Fatalf("planned: got %+v, want only the restored one", got)
		}
	})
}

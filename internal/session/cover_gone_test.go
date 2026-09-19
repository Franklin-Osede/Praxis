package session_test

import (
	"testing"

	"praxis/internal/market"
	"praxis/internal/session"
)

// stopOnly is a long of four contracts protected by a stop alone.
func stopOnly(t *testing.T, qty market.Qty) *session.Session {
	t.Helper()
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	mustProtect(t, s, order("entry", market.SideBuy, qty), 19_900, 0)
	return s
}

func endReasons(events []session.Event) []string {
	var out []string
	for _, e := range events {
		if v, ok := e.(session.ProtectionEnded); ok {
			out = append(out, v.Reason.String())
		}
	}
	return out
}

// Scenario: a protection whose last leg is cancelled with exposure still open
// ends, and says why
//
//	Given a long protected by a stop alone
//	When the stop triggers against a bid with nothing on it
//	Then the stop is cancelled, the protection ends as cover_gone, and the
//	  position is still open and uncovered.
//
// Until this it stayed Active holding no levels at all: a row the screen showed
// the participant as protection over a position that had none, and a state the
// live path refuses to be given directly, since a protection with both levels
// at zero is invalid. An aggregate goes when a ProtectionEnded says so, so the
// ending is recorded rather than folded.
func TestAStopTriggeredOnAnEmptyBookEndsTheProtection(t *testing.T) {
	s := stopOnly(t, 2)

	mustObserve(t, s, market.Quote{Instrument: mnq, Time: 4_000, Bid: 19_800, Ask: 19_801, BidSize: 0, AskSize: 50})

	if got := endReasons(s.Events()); len(got) != 1 || got[0] != "cover_gone" {
		t.Fatalf("endings %v, want one cover_gone", got)
	}
	if len(s.ActiveProtections()) != 0 {
		t.Fatalf("a protection with no levels outlived its legs: %+v", s.ActiveProtections())
	}
	if net := netQty(t, s); net != 2 {
		t.Fatalf("position %d, want the exposure to remain at 2", net)
	}
	checked(t, s)
}

// Scenario: a stop that fills part of the position takes the protection with it
//
// The remainder of a triggered stop is cancelled — it cannot untrigger — and
// with no target beside it the protection has no leg left over the exposure
// that remains.
func TestAPartiallyFilledStopEndsTheProtectionItEmptied(t *testing.T) {
	s := stopOnly(t, 4)

	mustObserve(t, s, sized(4_000, 19_800, 19_801, 2))

	if net := netQty(t, s); net != 2 {
		t.Fatalf("position %d, want 2 left after the book took 2", net)
	}
	if got := endReasons(s.Events()); len(got) != 1 || got[0] != "cover_gone" {
		t.Fatalf("endings %v, want one cover_gone", got)
	}
	if len(s.ActiveProtections()) != 0 {
		t.Fatalf("a protection with no levels outlived its legs: %+v", s.ActiveProtections())
	}
	checked(t, s)
}

// Scenario: a target still standing keeps the protection alive
//
// The complement, so the rule is "no leg left" and not "a stop went".
func TestAStopTriggeredBesideATargetLeavesTheProtectionStanding(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	mustProtect(t, s, order("entry", market.SideBuy, 4), 19_900, 20_400)

	mustObserve(t, s, sized(4_000, 19_800, 19_801, 2))

	if got := endReasons(s.Events()); len(got) != 0 {
		t.Fatalf("endings %v, want none while the target stands", got)
	}
	active := onlyActive(t, s)
	if active.TargetPrice != 20_400 || active.StopPrice != 0 || active.ProtectedQty != 2 {
		t.Fatalf("active %+v, want the target alone over 2", active)
	}
	checked(t, s)
}

// Scenario: a stop moved onto an empty bid ends the protection too
//
// Replacements now meet the book, so this is the second way into the state
// above and it must end the same way.
func TestAStopReplacedOntoAnEmptyBidEndsTheProtection(t *testing.T) {
	s := stopOnly(t, 2)
	mustObserve(t, s, market.Quote{Instrument: mnq, Time: 4_000, Bid: 19_950, Ask: 19_951, BidSize: 0, AskSize: 50})

	replaceActive(t, s, 19_950, 0)

	if got := endReasons(s.Events()); len(got) != 1 || got[0] != "cover_gone" {
		t.Fatalf("endings %v, want one cover_gone", got)
	}
	if net := netQty(t, s); net != 2 {
		t.Fatalf("position %d, want the exposure to remain at 2", net)
	}
	checked(t, s)
}

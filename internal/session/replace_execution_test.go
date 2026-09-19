package session_test

import (
	"reflect"
	"testing"

	"praxis/internal/challenge"
	"praxis/internal/market"
	"praxis/internal/session"
)

// replaceActive moves the levels of the only active protection.
func replaceActive(t *testing.T, s *session.Session, stop, target market.Ticks) {
	t.Helper()
	ref := episodeRef(onlyActive(t, s).EpisodeID)
	if err := s.ReplaceProtection(ref, stop, target, decidedAt()); err != nil {
		t.Fatalf("ReplaceProtection: %v", err)
	}
}

func sellFill(t *testing.T, events []session.Event) market.Fill {
	t.Helper()
	for _, e := range events {
		if v, ok := e.(session.FillProduced); ok && v.Fill.Side == market.SideSell {
			return v.Fill
		}
	}
	t.Fatal("no sell was filled")
	return market.Fill{}
}

// Scenario: a stop moved to where the market already is executes on the replace
//
//	Given a long protected by a stop below the bid
//	When the trader moves the stop up to the bid
//	Then the stop executes against the book on the screen, in the same batch
//	And the journal proves, reconstructs and resumes.
//
// A protection meets the observation that activated it, and a replacement is
// the same case: the new level stands against the book as it was left. Leaving
// it standing until the next observation wrote a journal the live session was
// content with and Replay refused, because a stop the price had reached was
// still waiting when the observation ended — the one thing a reader may never
// believe. Executing is the reading the rest of the engine takes; refusing the
// replacement would erase an act of exactly the kind the log exists to measure.
func TestAStopMovedToTheMarketExecutesOnTheReplace(t *testing.T) {
	s := protectedLong(t)
	before := s.JournalLen()

	replaceActive(t, s, 20_000, 20_400)

	if got, want := kinds(s.Events()[before:]), []session.Kind{
		session.KindProtectionReplaced,
		session.KindFillProduced, session.KindPositionChanged,
		session.KindOrderCancelled, session.KindProtectionEnded,
		session.KindAccountValued,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the batch is\n got: %v\nwant: %v", got, want)
	}
	if net := netQty(t, s); net != 0 {
		t.Fatalf("position: got %d, want the stop to have closed it", net)
	}
	if len(s.ActiveProtections()) != 0 {
		t.Fatal("a protection outlived the position it closed")
	}
	if fill := sellFill(t, s.Events()[before:]); fill.Price != 20_000 || fill.Qty != 10 {
		t.Fatalf("the stop filled %d at %d, want 10 at the bid of 20000", fill.Qty, fill.Price)
	}
	checked(t, s)
}

// Scenario: a target moved through the market executes at its own price
func TestATargetMovedThroughTheMarketExecutesOnTheReplace(t *testing.T) {
	s := protectedLong(t)
	before := s.JournalLen()

	replaceActive(t, s, 19_900, 19_990)

	if net := netQty(t, s); net != 0 {
		t.Fatalf("position: got %d, want the target to have closed it", net)
	}
	// A limit fills at its own price even where the bid is better.
	if fill := sellFill(t, s.Events()[before:]); fill.Price != 19_990 {
		t.Fatalf("the target filled at %d, want its own price of 19990", fill.Price)
	}
	checked(t, s)
}

// Scenario: a replacement the market has not reached records only itself
//
// Every journal written before this rule holds replacements of this kind, and
// they must keep the shape they have.
func TestAReplacementTheMarketHasNotReachedRecordsOnlyItself(t *testing.T) {
	s := protectedLong(t)
	before := s.JournalLen()

	replaceActive(t, s, 19_950, 20_300)

	if got, want := kinds(s.Events()[before:]), []session.Kind{session.KindProtectionReplaced}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the batch is\n got: %v\nwant: %v", got, want)
	}
	if active := onlyActive(t, s); active.StopPrice != 19_950 || active.ProtectedQty != 10 {
		t.Fatalf("active protection %+v, want a stop at 19950 over 10", active)
	}
	checked(t, s)
}

// Scenario: the market moving on after such a replacement still proves
func TestMarketAfterAStopMovedIntoTheBookStillProves(t *testing.T) {
	s := protectedLong(t)
	replaceActive(t, s, 20_000, 20_400)
	mustObserve(t, s, sized(4_000, 20_050, 20_051, 50))
	checked(t, s)
}

// Scenario: a replacement that ends the evaluation records it in its own batch
//
// A target that fills realises the gain, and a gain reads balance: the
// evaluation passes on the realised figure, inside the replacement's batch. A
// batch that stopped at the fill would leave an evaluation that had ended
// unrecorded until the next observation, and the valuation would be missing
// the fill that caused it.
func TestAReplacementThatEndsTheEvaluationRecordsItInTheSameBatch(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	mustProtect(t, s, order("entry", market.SideBuy, 10), 19_900, 20_900)
	// The mark is a gain and a gain reads balance, so the evaluation is still
	// running when the target is moved onto the bid.
	mustObserve(t, s, sized(4_000, 20_500, 20_501, 50))
	if state := s.Challenge().State(); state != challenge.StateActive {
		t.Fatalf("evaluation %v before the replacement, want active", state)
	}
	before := s.JournalLen()

	// Ten contracts from 20,001 to 20,500 is 249,500 cents less 500 in
	// commission, past a profit target of 100,000.
	replaceActive(t, s, 19_900, 20_500)

	if state := s.Challenge().State(); state != challenge.StatePassed {
		t.Fatalf("evaluation %v, want passed", state)
	}
	batch := kinds(s.Events()[before:])
	var decided bool
	for _, e := range s.Events()[before:] {
		if v, ok := e.(session.ChallengeDecision); ok && v.Decision.Kind == challenge.ChallengePassed {
			decided = true
		}
	}
	if !decided {
		t.Fatalf("the evaluation ended outside the replacement's batch: %v", batch)
	}
	if last := batch[len(batch)-1]; last != session.KindChallengeDecision && last != session.KindAccountValued {
		t.Fatalf("the batch ends with %v, want its valuation inside it: %v", last, batch)
	}
	checked(t, s)
}

// Scenario: a journal that puts the fill before the replacement is refused
//
// The fills belong to the replacement, so they follow it. A reader that only
// counted them would accept a log claiming the stop filled before the level it
// filled at existed.
func TestAFillBeforeTheReplacementThatCausedItIsRefused(t *testing.T) {
	s := protectedLong(t)
	replaceActive(t, s, 20_000, 20_400)

	events := s.Events()
	forged := make([]session.Event, len(events))
	copy(forged, events)
	i := indexOfKind(t, forged, session.KindProtectionReplaced, 1)
	j := indexOfKind(t, forged, session.KindFillProduced, 2)
	forged[i], forged[j] = restamped(t, forged[j], forged[i].Header()), restamped(t, forged[i], forged[j].Header())

	if _, err := session.Replay(forged); err == nil {
		t.Fatal("Replay accepted a fill recorded before the replacement that caused it")
	}
	// Verify holds the events and not the fills, so it cannot reach this one:
	// its obligations are a prefix of Replay's, which is the arrangement, not
	// an oversight. The claim is checked where the fills are.
}

// restamped moves an event to another position in the log, keeping what it says.
func restamped(t *testing.T, e session.Event, at session.Envelope) session.Event {
	t.Helper()
	switch v := e.(type) {
	case session.ProtectionReplaced:
		v.Envelope = session.Envelope{Time: at.Time, Sequence: at.Sequence, Kind: session.KindProtectionReplaced}
		return v
	case session.FillProduced:
		v.Envelope = session.Envelope{Time: at.Time, Sequence: at.Sequence, Kind: session.KindFillProduced}
		return v
	default:
		t.Fatalf("this forgery does not know how to move a %T", e)
		return nil
	}
}

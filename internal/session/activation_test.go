package session_test

import (
	"errors"
	"reflect"
	"testing"

	"praxis/internal/market"
	"praxis/internal/session"
)

func mustProtect(t *testing.T, s *session.Session, o market.Order, stop, target market.Ticks) {
	t.Helper()
	if err := s.SubmitOrderWithProtection(o, stop, target, decidedAt); err != nil {
		t.Fatalf("SubmitOrderWithProtection %s: %v", o.ID, err)
	}
}

func onlyActive(t *testing.T, s *session.Session) session.ActiveProtection {
	t.Helper()
	active := s.ActiveProtections()
	if len(active) != 1 {
		t.Fatalf("active protections: got %d, want 1", len(active))
	}
	return active[0]
}

// endings lists the protection endings a journal holds, as reference and reason.
func endings(events []session.Event) [][2]string {
	var out [][2]string
	for _, e := range events {
		if v, ok := e.(session.ProtectionEnded); ok {
			ref := "entry:" + v.Ref.OrderID
			if v.Ref.Kind == session.ProtectionRefEpisode {
				ref = "episode:" + itoa(v.Ref.EpisodeID)
			}
			out = append(out, [2]string{ref, v.Reason.String()})
		}
	}
	return out
}

// checked asserts the journal proves itself, reconstructs, and resumes holding
// exactly the protections the live session holds. Every case here ends with it:
// a protection that cannot survive a reconstruction is not a fact, it is a
// variable in one process's memory.
func checked(t *testing.T, s *session.Session) *session.ReplayedState {
	t.Helper()
	if err := session.Verify(s.Events()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	state, err := session.Replay(s.Events())
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if !reflect.DeepEqual(state.ActiveProtections, s.ActiveProtections()) {
		t.Fatalf("active\n got: %+v\nwant: %+v", state.ActiveProtections, s.ActiveProtections())
	}
	if !reflect.DeepEqual(state.PlannedProtections, s.PlannedProtections()) {
		t.Fatalf("planned\n got: %+v\nwant: %+v", state.PlannedProtections, s.PlannedProtections())
	}
	resumed, err := session.Resume(state, nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !reflect.DeepEqual(resumed.ActiveProtections(), s.ActiveProtections()) {
		t.Fatalf("a resumed session forgot what was protecting it: %+v", resumed.ActiveProtections())
	}
	if !reflect.DeepEqual(resumed.PlannedProtections(), s.PlannedProtections()) {
		t.Fatal("a resumed session forgot what was planned")
	}
	return state
}

// covered asserts the one thing that must never drift: a protection covers the
// exposure that is actually there.
func covered(t *testing.T, s *session.Session) {
	t.Helper()
	position, _ := s.Account().Position(mnq)
	net := position.NetQty
	if net < 0 {
		net = -net
	}
	active := s.ActiveProtections()
	if len(active) == 0 {
		return
	}
	if active[0].ProtectedQty != net {
		t.Fatalf("protected %d contracts of a position of %d", active[0].ProtectedQty, net)
	}
}

// opened is a session holding one long of two contracts, protected, activated
// by the fill that opened it.
func opened(t *testing.T) *session.Session {
	t.Helper()
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	mustProtect(t, s, order("entry", market.SideBuy, 2), 19_900, 20_400)
	return s
}

// Scenario: a fill that opens a position activates the plan placed with it
//
//	Given an entry submitted with levels
//	When it fills and opens an episode
//	Then the plan stops being planned, binds to that episode, keeps the names
//	  and levels it was placed with, and covers what opened.
func TestAFillThatOpensActivatesThePlan(t *testing.T) {
	s := opened(t)

	if planned := s.PlannedProtections(); len(planned) != 0 {
		t.Fatalf("the plan is still planned: %+v", planned)
	}
	active := onlyActive(t, s)
	if active.StopPrice != 19_900 || active.TargetPrice != 20_400 {
		t.Fatalf("levels: got %d/%d, want 19900/20400", active.StopPrice, active.TargetPrice)
	}
	if active.ProtectedQty != 2 || !active.Long {
		t.Fatalf("cover: got %d contracts, long=%v", active.ProtectedQty, active.Long)
	}

	// It is bound to the episode the change opened, which is that change's own
	// sequence — no registry, and the same number on every run.
	var openedAt uint64
	for _, e := range s.Events() {
		if v, ok := e.(session.PositionChanged); ok {
			openedAt = v.Sequence
			break
		}
	}
	if active.EpisodeID != openedAt {
		t.Fatalf("episode: got %d, want the sequence that opened it, %d", active.EpisodeID, openedAt)
	}
	// Activation is derived. It ends nothing and writes nothing.
	if got := endings(s.Events()); got != nil {
		t.Fatalf("activation recorded endings: %v", got)
	}
	covered(t, s)
	checked(t, s)
}

// Scenario: a plan arriving at an unprotected position covers all of it
//
// Protection binds to the episode, not to the fill that carried it. A trader
// who adds to a bare position with levels attached is protecting the position,
// and covering only the contracts that arrived with the plan would report cover
// they do not have on the rest.
func TestAPlanActivatingOnAnAdditionCoversTheWholePosition(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	mustSubmit(t, s, order("bare", market.SideBuy, 3))
	mustProtect(t, s, order("more", market.SideBuy, 2), 19_900, 20_400)

	if got := onlyActive(t, s).ProtectedQty; got != 5 {
		t.Fatalf("cover: got %d contracts, want the whole position of 5", got)
	}
	if got := endings(s.Events()); got != nil {
		t.Fatalf("recorded endings: %v", got)
	}
	covered(t, s)
	checked(t, s)
}

// Scenario: a plan arriving at a position that is already protected is refused
//
//	in the only honest way — out loud
//
// Keeping both would be protection per lot, which ADR-013 rejected. Replacing
// the old one silently would change the whole episode without naming the
// change. Ignoring the new one would make a recorded decision disappear. So the
// existing protection grows to the new exposure and the plan is ended saying
// why.
func TestAPlanMeetingAnActiveProtectionEndsSayingSo(t *testing.T) {
	s := opened(t)
	mustProtect(t, s, order("more", market.SideBuy, 3), 19_500, 20_900)

	active := onlyActive(t, s)
	if active.StopPrice != 19_900 || active.TargetPrice != 20_400 {
		t.Fatalf("levels: got %d/%d, want the protection already there", active.StopPrice, active.TargetPrice)
	}
	if active.ProtectedQty != 5 {
		t.Fatalf("cover: got %d contracts, want the grown position of 5", active.ProtectedQty)
	}
	if planned := s.PlannedProtections(); len(planned) != 0 {
		t.Fatalf("the refused plan is still planned: %+v", planned)
	}
	if got, want := endings(s.Events()), [][2]string{{"entry:more", "already_active"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("endings\n got: %v\nwant: %v", got, want)
	}
	covered(t, s)
	checked(t, s)
}

// Scenario: a reversal ends the old protection before the new one begins
//
//	Given a protected long
//	When one fill closes it and opens a short, carrying levels of its own
//	Then the batch reads: the close, the old protection ended as flipped, the
//	  open, and the new plan active over the new episode.
//
// This is why a fill's whole effect is assembled before any of it is written.
// Deciding on the close alone would end the arriving plan as having opened no
// exposure, one event before the exposure it opens.
func TestAReversalEndsTheOldProtectionThenActivatesTheNew(t *testing.T) {
	s := opened(t)
	before := s.JournalLen()
	mustProtect(t, s, order("flip", market.SideSell, 5), 20_100, 19_600)

	if got, want := kinds(s.Events()[before:]), []session.Kind{
		session.KindOrderSubmitted, session.KindProtectionPlaced,
		session.KindFillProduced,
		session.KindPositionChanged,
		// The legs of the protection the reversal undid go before it does,
		// and before the change that opens what replaces it.
		session.KindOrderCancelled, session.KindOrderCancelled,
		session.KindProtectionEnded,
		session.KindPositionChanged,
		session.KindAccountValued,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the batch is\n got: %v\nwant: %v", got, want)
	}

	ended := endings(s.Events())
	if len(ended) != 1 || ended[0][1] != "flipped" {
		t.Fatalf("endings: got %v, want the old protection ended as flipped", ended)
	}

	active := onlyActive(t, s)
	if active.StopPrice != 20_100 || active.TargetPrice != 19_600 || active.Long {
		t.Fatalf("the new protection: got %+v", active)
	}
	if active.ProtectedQty != 3 {
		t.Fatalf("cover: got %d contracts, want the 3 the reversal opened", active.ProtectedQty)
	}
	// The old episode's protection is gone, and its identity was named.
	if ended[0][0] == "entry:flip" {
		t.Fatal("the reversal ended the arriving plan instead of the protection it replaced")
	}
	covered(t, s)
	checked(t, s)
}

// Scenario: a plan that opened no exposure ends saying so
//
// Levels attached to an order that turned out to reduce or close a position
// protect nothing. There is no episode of theirs to bind to, and carrying them
// would leave a plan waiting for an entry that has already had its effect.
func TestAPlanOnAnExitEndsHavingOpenedNothing(t *testing.T) {
	t.Run("an exit that reduces", func(t *testing.T) {
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
		mustSubmit(t, s, order("bare", market.SideBuy, 4))
		mustProtect(t, s, order("exit", market.SideSell, 1), 20_100, 19_600)

		if got, want := endings(s.Events()), [][2]string{{"entry:exit", "did_not_open_exposure"}}; !reflect.DeepEqual(got, want) {
			t.Fatalf("endings\n got: %v\nwant: %v", got, want)
		}
		if len(s.ActiveProtections()) != 0 || len(s.PlannedProtections()) != 0 {
			t.Fatal("a plan that opened nothing survived")
		}
		checked(t, s)
	})

	t.Run("an exit that closes", func(t *testing.T) {
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
		mustSubmit(t, s, order("bare", market.SideBuy, 4))
		mustProtect(t, s, order("exit", market.SideSell, 4), 20_100, 19_600)

		if got, want := endings(s.Events()), [][2]string{{"entry:exit", "did_not_open_exposure"}}; !reflect.DeepEqual(got, want) {
			t.Fatalf("endings\n got: %v\nwant: %v", got, want)
		}
		checked(t, s)
	})
}

// Scenario: an exit that closes a protected position ends its protection
func TestClosingAProtectedPositionEndsItsProtection(t *testing.T) {
	s := opened(t)
	episode := onlyActive(t, s).EpisodeID
	mustSubmit(t, s, order("exit", market.SideSell, 2))

	if got, want := endings(s.Events()), [][2]string{{"episode:" + itoa(episode), "position_closed"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("endings\n got: %v\nwant: %v", got, want)
	}
	if len(s.ActiveProtections()) != 0 {
		t.Fatal("a protection outlived the position it was on")
	}
	checked(t, s)
}

// A manual partial exit shrinks the cover. Protection can never close contracts
// that are no longer there.
func TestAPartialExitShrinksTheCover(t *testing.T) {
	s := opened(t)
	mustSubmit(t, s, order("some", market.SideBuy, 3))
	covered(t, s)
	mustSubmit(t, s, order("exit", market.SideSell, 4))

	if got := onlyActive(t, s).ProtectedQty; got != 1 {
		t.Fatalf("cover: got %d contracts, want the 1 still open", got)
	}
	if got := endings(s.Events()); got != nil {
		t.Fatalf("a partial exit ended something: %v", got)
	}
	covered(t, s)
	checked(t, s)
}

func episodeRef(id uint64) session.ProtectionRef {
	return session.ProtectionRef{Kind: session.ProtectionRefEpisode, EpisodeID: id}
}

// Scenario: a later fill of the same entry grows the cover and nothing else
//
//	Given a protected entry that filled in part and is still working
//	And a replacement made against the episode it activated
//	When the remainder fills
//	Then the replaced levels stand and only the quantity grows.
//
// Without this, a replacement made after a partial fill would be undone by the
// next fill of the same entry: the trader would see the levels they had already
// moved away from put back by an event they did not cause.
func TestALaterFillOfTheSameEntryOnlyGrowsTheCover(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 2))
	mustProtect(t, s, limitOrder(t, "entry", market.SideBuy, 5, 20_100), 19_900, 20_400)

	active := onlyActive(t, s)
	if active.ProtectedQty != 2 {
		t.Fatalf("cover: got %d contracts, want the 2 that filled", active.ProtectedQty)
	}
	if len(s.WorkingOrders()) != 1 {
		t.Fatalf("working: got %+v, want the remainder still waiting", s.WorkingOrders())
	}

	// Once active the episode governs, and the entry's name is stale.
	if err := s.ReplaceProtection(entryRef("entry"), 19_800, 0, decidedAt); err == nil {
		t.Fatal("a protection that has activated answered to its entry")
	}
	if err := s.ReplaceProtection(episodeRef(active.EpisodeID), 19_800, 0, decidedAt); err != nil {
		t.Fatalf("ReplaceProtection: %v", err)
	}

	mustObserve(t, s, sized(4_000, 20_000, 20_001, 50))

	grown := onlyActive(t, s)
	if grown.EpisodeID != active.EpisodeID {
		t.Fatal("the remainder started a second protection")
	}
	if grown.StopPrice != 19_800 || grown.TargetPrice != 0 || grown.TargetOrderID != "" {
		t.Fatalf("levels: got %+v, want the replacement to stand", grown)
	}
	if grown.ProtectedQty != 5 {
		t.Fatalf("cover: got %d contracts, want the whole 5", grown.ProtectedQty)
	}
	if got := endings(s.Events()); got != nil {
		t.Fatalf("the remainder ended something: %v", got)
	}
	covered(t, s)
	checked(t, s)
}

// Scenario: activation carries what the plan holds now, not what it was placed
// with
//
// A level withdrawn while the entry waited is withdrawn. Restoring it at the
// moment of the fill would hand the trader back a decision they had already
// reversed, and an identifier they had already stopped using.
func TestActivationNeverRestoresAWithdrawnLevel(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	mustProtect(t, s, limitOrder(t, "entry", market.SideBuy, 2, 19_000), 18_900, 19_500)

	planned := onlyPlanned(t, s)
	targetName := planned.TargetOrderID
	if targetName == "" {
		t.Fatal("the fixture has no target to withdraw")
	}
	if err := s.ReplaceProtection(entryRef("entry"), 18_800, 0, decidedAt); err != nil {
		t.Fatalf("ReplaceProtection: %v", err)
	}

	// The limit is now reachable, so the entry fills and the plan activates.
	mustObserve(t, s, sized(4_000, 18_990, 18_991, 50))

	active := onlyActive(t, s)
	if active.StopPrice != 18_800 || active.TargetPrice != 0 {
		t.Fatalf("levels: got %d/%d, want the levels as they stood", active.StopPrice, active.TargetPrice)
	}
	if active.TargetOrderID != "" {
		t.Fatalf("activation restored the withdrawn target's name %q", active.TargetOrderID)
	}
	if active.StopOrderID != planned.StopOrderID {
		t.Fatalf("the surviving stop was renamed: %q became %q", planned.StopOrderID, active.StopOrderID)
	}
	// And the withdrawn name is still spent: it was used, so it never comes back.
	if err := s.SubmitOrder(order(targetName, market.SideBuy, 1), decidedAt); err == nil {
		t.Fatal("a withdrawn protective name was handed back")
	}
	covered(t, s)
	checked(t, s)
}

// Scenario: cancelling what is left of an entry does not touch what it opened
//
// The plan activated at the first fill, so the entry stopped governing it. The
// remainder disappearing says nothing about the contracts already open, and
// ending their cover would leave a position the trader believes is protected
// and is not.
func TestCancellingTheRemainderKeepsTheActiveProtection(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 2))
	mustProtect(t, s, limitOrder(t, "entry", market.SideBuy, 5, 20_100), 19_900, 20_400)
	before := s.JournalLen()

	if err := s.CancelOrder("entry", decidedAt); err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	if got, want := kinds(s.Events()[before:]), []session.Kind{session.KindOrderCancelled}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the batch is\n got: %v\nwant: %v", got, want)
	}

	active := onlyActive(t, s)
	if active.ProtectedQty != 2 || active.StopPrice != 19_900 {
		t.Fatalf("the protection changed: %+v", active)
	}
	covered(t, s)
	checked(t, s)
}

// Scenario: a protection survives a journal in each state it can be in
func TestAProtectionReconstructsInEveryState(t *testing.T) {
	tests := []struct {
		name string
		act  func(*testing.T, *session.Session)
	}{
		{"planned", func(t *testing.T, s *session.Session) {
			mustProtect(t, s, limitOrder(t, "entry", market.SideBuy, 2, 19_000), 18_900, 19_500)
		}},
		{"active with both legs", func(t *testing.T, s *session.Session) {
			mustProtect(t, s, order("entry", market.SideBuy, 2), 19_900, 20_400)
		}},
		{"active with one leg", func(t *testing.T, s *session.Session) {
			mustProtect(t, s, order("entry", market.SideBuy, 2), 19_900, 0)
		}},
		{"active, then reduced to one leg", func(t *testing.T, s *session.Session) {
			mustProtect(t, s, order("entry", market.SideBuy, 2), 19_900, 20_400)
			if err := s.ReplaceProtection(episodeRef(onlyActive(t, s).EpisodeID), 0, 20_500, decidedAt); err != nil {
				t.Fatalf("ReplaceProtection: %v", err)
			}
		}},
		{"ended with the position", func(t *testing.T, s *session.Session) {
			mustProtect(t, s, order("entry", market.SideBuy, 2), 19_900, 20_400)
			mustSubmit(t, s, order("exit", market.SideSell, 2))
		}},
		{"ended by the trader while active", func(t *testing.T, s *session.Session) {
			mustProtect(t, s, order("entry", market.SideBuy, 2), 19_900, 20_400)
			if err := s.CancelProtection(episodeRef(onlyActive(t, s).EpisodeID), decidedAt); err != nil {
				t.Fatalf("CancelProtection: %v", err)
			}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newSession(t)
			mustOpen(t, s, 2_000, "d1")
			mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
			tc.act(t, s)

			state := checked(t, s)
			// A resumed session carries on producing what an uninterrupted one
			// would: the names already spent are still spent.
			resumed, err := session.Resume(state, nil)
			if err != nil {
				t.Fatalf("Resume: %v", err)
			}
			if err := resumed.SubmitOrder(order("entry", market.SideBuy, 1), decidedAt); err == nil {
				t.Fatal("a resumed session reused a name")
			}
		})
	}
}

// Property: the same commands produce the same journal, the same episode
// identities and the same protective names, on every run.
func TestPropertyActivationIsDeterministic(t *testing.T) {
	build := func(t *testing.T) *session.Session {
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
		mustProtect(t, s, order("entry", market.SideBuy, 2), 19_900, 20_400)
		mustProtect(t, s, order("more", market.SideBuy, 3), 19_500, 20_900)
		mustProtect(t, s, order("flip", market.SideSell, 8), 20_100, 19_600)
		return s
	}

	baseline := build(t)
	for run := 1; run < 20; run++ {
		again := build(t)
		if !reflect.DeepEqual(again.Events(), baseline.Events()) {
			t.Fatalf("run %d produced a different journal", run)
		}
		if !reflect.DeepEqual(again.ActiveProtections(), baseline.ActiveProtections()) {
			t.Fatalf("run %d protected differently: %+v", run, again.ActiveProtections())
		}
	}
	covered(t, baseline)
	checked(t, baseline)
}

// Scenario: a journal in which a closed episode kept its protection is refused
//
// This is the one thing about a fill's protection that a reader without the
// fills can still insist on. Verify cannot know whether a close was a reversal
// or an exit — that is what the fill decides, and Replay proves it — but it can
// see that an episode ended, and that nothing may be left protecting it.
func TestAClosedEpisodeCannotKeepItsProtection(t *testing.T) {
	s := opened(t)
	mustSubmit(t, s, order("exit", market.SideSell, 2))
	events := s.Events()

	at := indexOfKind(t, events, session.KindProtectionEnded, 1)
	header := events[at].Header()
	// A structurally impeccable event in its place, at the right sequence and
	// time, naming an order nothing is waiting for.
	events[at] = session.OrderCancelled{
		Envelope: session.Envelope{
			Time: header.Time, Sequence: header.Sequence, Kind: session.KindOrderCancelled,
		},
		OrderID: "somebody-else", RemainingQty: 1,
		// Stamped with a person's clock, because a cancellation the trader
		// asked for and nobody's moment is refused before this gets read.
		Reason: session.CancelledByTrader, Decided: decidedAt,
	}

	if err := session.Verify(events); !errors.Is(err, session.ErrContradictoryLog) {
		t.Fatalf("Verify: got %v, want %v", err, session.ErrContradictoryLog)
	}
	if _, err := session.Replay(events); !errors.Is(err, session.ErrFabricated) {
		t.Fatalf("Replay: got %v, want %v", err, session.ErrFabricated)
	}
}

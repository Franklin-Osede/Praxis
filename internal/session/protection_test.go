package session_test

import (
	"errors"
	"reflect"
	"testing"

	"praxis/internal/market"
	"praxis/internal/session"
)

func entryRef(id string) session.ProtectionRef {
	return session.ProtectionRef{Kind: session.ProtectionRefEntry, OrderID: id}
}

// protectedSession is a session with a resting limit entry that carries a stop
// and a target. The entry is deliberately far from the market so that nothing
// fills: this slice is about a protection whose entry is still waiting.
func protectedSession(t *testing.T, stop, target market.Ticks) *session.Session {
	t.Helper()
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	if err := s.SubmitOrderWithProtection(limitOrder(t, "entry", market.SideBuy, 2, 19_000), stop, target); err != nil {
		t.Fatalf("SubmitOrderWithProtection: %v", err)
	}
	return s
}

func onlyPlanned(t *testing.T, s *session.Session) session.PlannedProtection {
	t.Helper()
	planned := s.PlannedProtections()
	if len(planned) != 1 {
		t.Fatalf("planned protections: got %d, want 1", len(planned))
	}
	return planned[0]
}

// Scenario: an entry and the levels planned with it are one decision
//
//	Given an order submitted with a stop and a target
//	Then both are recorded, in one batch, and each level that exists is
//	  given a name the system owns.
//
// They are one decision. Recording them separately would allow a journal in
// which the entry exists and its protection does not — a state the trader never
// chose and the log could not explain.
func TestSubmittingAnOrderWithProtection(t *testing.T) {
	s := protectedSession(t, 18_900, 19_500)

	planned := onlyPlanned(t, s)
	if planned.EntryOrderID != "entry" || planned.StopPrice != 18_900 || planned.TargetPrice != 19_500 {
		t.Fatalf("planned: got %+v", planned)
	}

	var placed session.ProtectionPlaced
	for _, e := range s.Events() {
		if p, ok := e.(session.ProtectionPlaced); ok {
			placed = p
		}
	}
	if placed.StopOrderID == "" || placed.TargetOrderID == "" {
		t.Fatalf("placed: got %+v, want a name for each level", placed)
	}
	// The names come from this event's own sequence, not from a count of how
	// many events the command was expected to produce.
	if want := "praxis:" + itoa(placed.Sequence) + ":stop"; placed.StopOrderID != want {
		t.Fatalf("stop name: got %q, want %q", placed.StopOrderID, want)
	}
	if planned.StopOrderID != placed.StopOrderID || planned.TargetOrderID != placed.TargetOrderID {
		t.Fatal("the projection and the event disagree about the names")
	}
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var digits []byte
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits)
}

// Scenario: a level that is not set reserves nothing
func TestOnlyALevelThatExistsIsNamed(t *testing.T) {
	t.Run("a stop alone", func(t *testing.T) {
		planned := onlyPlanned(t, protectedSession(t, 18_900, 0))
		if planned.StopOrderID == "" || planned.TargetOrderID != "" {
			t.Fatalf("planned: got %+v, want only a stop named", planned)
		}
	})
	t.Run("a target alone", func(t *testing.T) {
		planned := onlyPlanned(t, protectedSession(t, 0, 19_500))
		if planned.TargetOrderID == "" || planned.StopOrderID != "" {
			t.Fatalf("planned: got %+v, want only a target named", planned)
		}
	})
}

// Scenario: a protection with neither level protects nothing
//
// And a refusal leaves no order, no protection, no reserved name, no counter
// moved and no event.
func TestAProtectionWithNoLevelsIsRefusedWithoutTrace(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))

	before := s.JournalLen()
	err := s.SubmitOrderWithProtection(limitOrder(t, "entry", market.SideBuy, 2, 19_000), 0, 0)
	if !errors.Is(err, session.ErrProtectionEmpty) {
		t.Fatalf("error: got %v, want %v", err, session.ErrProtectionEmpty)
	}
	if s.JournalLen() != before {
		t.Fatal("a refused command was recorded")
	}
	if len(s.WorkingOrders()) != 0 || len(s.PlannedProtections()) != 0 {
		t.Fatal("a refused command left something behind")
	}

	// And the name is still free, so nothing was reserved.
	if err := s.SubmitOrderWithProtection(limitOrder(t, "entry", market.SideBuy, 2, 19_000), 18_900, 0); err != nil {
		t.Fatalf("the identifier was consumed by a refusal: %v", err)
	}
}

// An order the session would refuse anyway reserves nothing either.
func TestARefusedOrderReservesNoNames(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	if err := s.SubmitOrderWithProtection(limitOrder(t, "entry", market.SideBuy, 2, 19_000), 18_900, 0); err != nil {
		t.Fatalf("SubmitOrderWithProtection: %v", err)
	}

	before := s.JournalLen()
	if err := s.SubmitOrderWithProtection(limitOrder(t, "entry", market.SideBuy, 1, 19_000), 18_800, 0); !errors.Is(err, session.ErrDuplicateOrderID) {
		t.Fatalf("error: got %v, want %v", err, session.ErrDuplicateOrderID)
	}
	if s.JournalLen() != before || len(s.PlannedProtections()) != 1 {
		t.Fatal("a refused duplicate changed the session")
	}
}

// Scenario: a planned protection can be moved while its entry waits
//
// Moving a stop before the entry fills is ordinary, and it is the behaviour
// worth measuring. Making the trader cancel and resubmit the whole entry would
// erase the very decision the experiment is about.
func TestChangingAPlannedProtection(t *testing.T) {
	s := protectedSession(t, 18_900, 19_500)

	if err := s.ReplaceProtection(entryRef("entry"), 18_800, 19_500); err != nil {
		t.Fatalf("ReplaceProtection: %v", err)
	}
	planned := onlyPlanned(t, s)
	if planned.StopPrice != 18_800 {
		t.Fatalf("planned: got %+v, want the stop moved", planned)
	}

	var replaced session.ProtectionReplaced
	for _, e := range s.Events() {
		if r, ok := e.(session.ProtectionReplaced); ok {
			replaced = r
		}
	}
	if replaced.PreviousStopPrice != 18_900 || replaced.PreviousTargetPrice != 19_500 {
		t.Fatalf("previous levels: got %d/%d", replaced.PreviousStopPrice, replaced.PreviousTargetPrice)
	}
	if !replaced.Widened {
		t.Fatal("a long's stop moved lower is a widening")
	}
	if replaced.StopOrderID != planned.StopOrderID {
		t.Fatal("a level that survived was given a new name")
	}
	// The entry is untouched: only the protection changed.
	if len(s.WorkingOrders()) != 1 {
		t.Fatal("the entry stopped waiting")
	}
	if err := session.Verify(s.Events()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// Placing a level that was not there is placing it, not widening it.
func TestReplacingFromNothingIsNotWidening(t *testing.T) {
	s := protectedSession(t, 0, 19_500)

	if err := s.ReplaceProtection(entryRef("entry"), 18_900, 19_500); err != nil {
		t.Fatalf("ReplaceProtection: %v", err)
	}
	for _, e := range s.Events() {
		if r, ok := e.(session.ProtectionReplaced); ok && r.Widened {
			t.Fatal("placing a stop from nothing was recorded as a widening")
		}
	}
	planned := onlyPlanned(t, s)
	if planned.StopOrderID == "" {
		t.Fatal("the new level was not named")
	}
}

// Scenario: withdrawing a protection leaves its entry alone
func TestWithdrawingAProtectionKeepsTheEntry(t *testing.T) {
	s := protectedSession(t, 18_900, 19_500)

	if err := s.CancelProtection(entryRef("entry")); err != nil {
		t.Fatalf("CancelProtection: %v", err)
	}
	if len(s.PlannedProtections()) != 0 {
		t.Fatal("the protection is still planned")
	}
	if len(s.WorkingOrders()) != 1 {
		t.Fatal("withdrawing the protection also withdrew the entry")
	}

	var ended session.ProtectionEnded
	for _, e := range s.Events() {
		if p, ok := e.(session.ProtectionEnded); ok {
			ended = p
		}
	}
	if ended.Reason != session.ProtectionWithdrawnByTrader {
		t.Fatalf("reason: got %v", ended.Reason)
	}
	if ended.StopPrice != 18_900 || ended.TargetPrice != 19_500 {
		t.Fatalf("levels: got %d/%d, want what was withdrawn", ended.StopPrice, ended.TargetPrice)
	}
}

// Scenario: a protection that no longer exists cannot be acted on
func TestActingOnAWithdrawnProtection(t *testing.T) {
	s := protectedSession(t, 18_900, 19_500)
	if err := s.CancelProtection(entryRef("entry")); err != nil {
		t.Fatalf("CancelProtection: %v", err)
	}
	before := s.JournalLen()

	if err := s.CancelProtection(entryRef("entry")); !errors.Is(err, session.ErrNoSuchProtection) {
		t.Fatalf("cancelling twice: got %v, want %v", err, session.ErrNoSuchProtection)
	}
	if err := s.ReplaceProtection(entryRef("entry"), 18_800, 0); !errors.Is(err, session.ErrNoSuchProtection) {
		t.Fatalf("replacing after cancelling: got %v, want %v", err, session.ErrNoSuchProtection)
	}
	if s.JournalLen() != before {
		t.Fatal("a refused command was recorded")
	}
}

// Replacing both levels with nothing is withdrawing, and must be said as that.
func TestReplacingWithNothingIsRefused(t *testing.T) {
	s := protectedSession(t, 18_900, 19_500)
	before := s.JournalLen()

	if err := s.ReplaceProtection(entryRef("entry"), 0, 0); !errors.Is(err, session.ErrProtectionEmpty) {
		t.Fatalf("error: got %v, want %v", err, session.ErrProtectionEmpty)
	}
	if s.JournalLen() != before || onlyPlanned(t, s).StopPrice != 18_900 {
		t.Fatal("a refused replacement changed the protection")
	}
}

// Scenario: a name is never reused, and the system's namespace is its own
func TestIdentifiersAreSpentForever(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))

	// An order that finishes still owns its name.
	mustSubmit(t, s, order("used", market.SideBuy, 1))
	if err := s.SubmitOrder(order("used", market.SideSell, 1)); !errors.Is(err, session.ErrOrderIDReused) {
		t.Fatalf("reusing a finished name: got %v, want %v", err, session.ErrOrderIDReused)
	}

	if err := s.SubmitOrder(limitOrder(t, "praxis:1:stop", market.SideBuy, 1, 19_000)); !errors.Is(err, session.ErrReservedNamespace) {
		t.Fatalf("the reserved namespace: got %v, want %v", err, session.ErrReservedNamespace)
	}
}

// Scenario: a planned protection survives a journal in each of its states
func TestAPlannedProtectionReplaysAndResumes(t *testing.T) {
	tests := []struct {
		name string
		act  func(*testing.T, *session.Session)
	}{
		{"as placed", func(*testing.T, *session.Session) {}},
		{"after a change", func(t *testing.T, s *session.Session) {
			if err := s.ReplaceProtection(entryRef("entry"), 18_800, 0); err != nil {
				t.Fatalf("ReplaceProtection: %v", err)
			}
		}},
		{"after withdrawal", func(t *testing.T, s *session.Session) {
			if err := s.CancelProtection(entryRef("entry")); err != nil {
				t.Fatalf("CancelProtection: %v", err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := protectedSession(t, 18_900, 19_500)
			tc.act(t, s)

			if err := session.Verify(s.Events()); err != nil {
				t.Fatalf("Verify: %v", err)
			}
			state, err := session.Replay(s.Events())
			if err != nil {
				t.Fatalf("Replay: %v", err)
			}
			if !reflect.DeepEqual(state.PlannedProtections, s.PlannedProtections()) {
				t.Fatalf("planned\n got: %+v\nwant: %+v", state.PlannedProtections, s.PlannedProtections())
			}

			resumed, err := session.Resume(state, nil)
			if err != nil {
				t.Fatalf("Resume: %v", err)
			}
			if !reflect.DeepEqual(resumed.PlannedProtections(), s.PlannedProtections()) {
				t.Fatal("a resumed session forgot what was planned")
			}
			// And the names it already spent are still spent.
			if err := resumed.SubmitOrder(order("entry", market.SideBuy, 1)); err == nil {
				t.Fatal("a resumed session reused a name")
			}
		})
	}
}

// Property: the same commands produce the same journal, every time.
func TestPropertyProtectionCommandsAreDeterministic(t *testing.T) {
	build := func() []session.Event {
		s := protectedSession(t, 18_900, 19_500)
		if err := s.ReplaceProtection(entryRef("entry"), 18_800, 19_600); err != nil {
			t.Fatalf("ReplaceProtection: %v", err)
		}
		if err := s.CancelProtection(entryRef("entry")); err != nil {
			t.Fatalf("CancelProtection: %v", err)
		}
		return s.Events()
	}

	baseline := build()
	for run := 1; run < 20; run++ {
		if !reflect.DeepEqual(build(), baseline) {
			t.Fatalf("run %d produced a different journal", run)
		}
	}
}

// A protection command needs an open session and an observed book, like every
// other command.
func TestProtectionCommandsNeedASession(t *testing.T) {
	s := newSession(t)
	if err := s.SubmitOrderWithProtection(limitOrder(t, "entry", market.SideBuy, 1, 19_000), 18_900, 0); !errors.Is(err, session.ErrNoSessionOpen) {
		t.Fatalf("error: got %v, want %v", err, session.ErrNoSessionOpen)
	}
	if err := s.CancelProtection(entryRef("entry")); !errors.Is(err, session.ErrNoSessionOpen) {
		t.Fatalf("error: got %v, want %v", err, session.ErrNoSessionOpen)
	}
}

// An episode reference is refused with a message that says why, rather than
// pretending it works.
func TestAnEpisodeReferenceIsNotYetPossible(t *testing.T) {
	s := protectedSession(t, 18_900, 0)
	ref := session.ProtectionRef{Kind: session.ProtectionRefEpisode, EpisodeID: 7}
	if err := s.ReplaceProtection(ref, 18_800, 0); !errors.Is(err, session.ErrProtectionNotPlanned) {
		t.Fatalf("error: got %v, want %v", err, session.ErrProtectionNotPlanned)
	}
}

// A reference that names neither an entry nor an episode is refused.
func TestAMalformedReferenceIsRefused(t *testing.T) {
	s := protectedSession(t, 18_900, 0)
	for _, ref := range []session.ProtectionRef{
		{},
		{Kind: session.ProtectionRefEntry},
		{Kind: session.ProtectionRefEntry, OrderID: "entry", EpisodeID: 3},
		{Kind: session.ProtectionRefEpisode},
		{Kind: session.ProtectionRefEpisode, EpisodeID: 3, OrderID: "entry"},
	} {
		if err := s.CancelProtection(ref); !errors.Is(err, session.ErrProtectionRef) {
			t.Fatalf("%+v: got %v, want %v", ref, err, session.ErrProtectionRef)
		}
	}
}

// Scenario: the derived parts of a change are checked, not believed
//
//	Given a log whose recorded previous levels, or whose widened flag, were
//	  altered
//	When it is verified
//	Then it is refused. Both are derivable from the events before them, and a
//	  log that can contradict itself about a decision is worse than one that
//	  stores less.
func TestVerifyChecksTheDerivedPartsOfAChange(t *testing.T) {
	build := func(t *testing.T) []session.Event {
		t.Helper()
		s := protectedSession(t, 18_900, 19_500)
		if err := s.ReplaceProtection(entryRef("entry"), 18_800, 19_500); err != nil {
			t.Fatalf("ReplaceProtection: %v", err)
		}
		return s.Events()
	}

	tests := []struct {
		name  string
		alter func(session.ProtectionReplaced) session.ProtectionReplaced
	}{
		{"a previous stop that was never there", func(r session.ProtectionReplaced) session.ProtectionReplaced {
			r.PreviousStopPrice = 18_950
			return r
		}},
		{"a previous target that was never there", func(r session.ProtectionReplaced) session.ProtectionReplaced {
			r.PreviousTargetPrice = 19_400
			return r
		}},
		{"a widening that did not happen", func(r session.ProtectionReplaced) session.ProtectionReplaced {
			r.Widened = false
			return r
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events := build(t)
			if err := session.Verify(events); err != nil {
				t.Fatalf("the honest log does not verify: %v", err)
			}
			altered := false
			for n, e := range events {
				if r, ok := e.(session.ProtectionReplaced); ok && !altered {
					events[n], altered = tc.alter(r), true
				}
			}
			if !altered {
				t.Fatal("there was no change to alter")
			}
			if err := session.Verify(events); !errors.Is(err, session.ErrContradictoryLog) {
				t.Fatalf("error: got %v, want %v", err, session.ErrContradictoryLog)
			}
		})
	}
}

// A stop moved toward the entry is not a widening, so the flag must be able to
// be false honestly and not only by omission.
func TestTighteningAStopIsNotAWidening(t *testing.T) {
	s := protectedSession(t, 18_900, 19_500)
	if err := s.ReplaceProtection(entryRef("entry"), 18_950, 19_500); err != nil {
		t.Fatalf("ReplaceProtection: %v", err)
	}
	for _, e := range s.Events() {
		if r, ok := e.(session.ProtectionReplaced); ok && r.Widened {
			t.Fatal("moving a long's stop up was recorded as a widening")
		}
	}
	if err := session.Verify(s.Events()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

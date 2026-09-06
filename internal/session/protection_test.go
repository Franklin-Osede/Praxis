package session_test

import (
	"errors"
	"math"
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
	if err := s.SubmitOrderWithProtection(limitOrder(t, "entry", market.SideBuy, 2, 19_000), stop, target, decidedAt); err != nil {
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
	err := s.SubmitOrderWithProtection(limitOrder(t, "entry", market.SideBuy, 2, 19_000), 0, 0, decidedAt)
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
	if err := s.SubmitOrderWithProtection(limitOrder(t, "entry", market.SideBuy, 2, 19_000), 18_900, 0, decidedAt); err != nil {
		t.Fatalf("the identifier was consumed by a refusal: %v", err)
	}
}

// An order the session would refuse anyway reserves nothing either.
func TestARefusedOrderReservesNoNames(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	if err := s.SubmitOrderWithProtection(limitOrder(t, "entry", market.SideBuy, 2, 19_000), 18_900, 0, decidedAt); err != nil {
		t.Fatalf("SubmitOrderWithProtection: %v", err)
	}

	before := s.JournalLen()
	if err := s.SubmitOrderWithProtection(limitOrder(t, "entry", market.SideBuy, 1, 19_000), 18_800, 0, decidedAt); !errors.Is(err, session.ErrDuplicateOrderID) {
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

	if err := s.ReplaceProtection(entryRef("entry"), 18_800, 19_500, decidedAt); err != nil {
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

	if err := s.ReplaceProtection(entryRef("entry"), 18_900, 19_500, decidedAt); err != nil {
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

	if err := s.CancelProtection(entryRef("entry"), decidedAt); err != nil {
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
	if err := s.CancelProtection(entryRef("entry"), decidedAt); err != nil {
		t.Fatalf("CancelProtection: %v", err)
	}
	before := s.JournalLen()

	if err := s.CancelProtection(entryRef("entry"), decidedAt); !errors.Is(err, session.ErrNoSuchProtection) {
		t.Fatalf("cancelling twice: got %v, want %v", err, session.ErrNoSuchProtection)
	}
	if err := s.ReplaceProtection(entryRef("entry"), 18_800, 0, decidedAt); !errors.Is(err, session.ErrNoSuchProtection) {
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

	if err := s.ReplaceProtection(entryRef("entry"), 0, 0, decidedAt); !errors.Is(err, session.ErrProtectionEmpty) {
		t.Fatalf("error: got %v, want %v", err, session.ErrProtectionEmpty)
	}
	if s.JournalLen() != before || onlyPlanned(t, s).StopPrice != 18_900 {
		t.Fatal("a refused replacement changed the protection")
	}
}

// Scenario: the two levels must be on the right sides of each other
//
// A long protected with its stop above its target has the legs doing each
// other's job, and both can be reachable in one observation — at which point
// which of them executes depends on the order the engine visits them in, and
// the log records an outcome the trader could not have predicted from what
// they placed. Equality is the same defect with no gap in it.
func TestLevelsOnTheWrongSidesAreRefused(t *testing.T) {
	tests := []struct {
		name         string
		side         market.Side
		stop, target market.Ticks
		want         error
	}{
		{"a long with the stop above the target", market.SideBuy, 20_500, 19_500, session.ErrProtectionInverted},
		{"a short with the target above the stop", market.SideSell, 19_500, 20_500, session.ErrProtectionInverted},
		{"levels that meet", market.SideBuy, 20_000, 20_000, session.ErrProtectionInverted},
		{"a long the right way round", market.SideBuy, 19_500, 20_500, nil},
		{"a short the right way round", market.SideSell, 20_500, 19_500, nil},
		// One level alone has nothing to be on the wrong side of.
		{"a stop alone, wherever it is", market.SideBuy, 20_500, 0, nil},
		{"a target alone, wherever it is", market.SideBuy, 0, 19_500, nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newSession(t)
			mustOpen(t, s, 2_000, "d1")
			mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
			before := s.JournalLen()

			// A limit far from the market, so nothing fills and this is about
			// the levels alone.
			limit := market.Ticks(19_000)
			if tc.side == market.SideSell {
				limit = 21_000
			}
			err := s.SubmitOrderWithProtection(limitOrder(t, "entry", tc.side, 2, limit), tc.stop, tc.target, decidedAt)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error: got %v, want %v", err, tc.want)
			}
			if tc.want == nil {
				return
			}
			if s.JournalLen() != before {
				t.Fatal("a refused command was recorded")
			}
			if len(s.WorkingOrders()) != 0 || len(s.PlannedProtections()) != 0 {
				t.Fatal("a refused command left something behind")
			}
		})
	}
}

// A replacement is held to the same geometry, against the side the entry would
// open — not against the side of whoever asked.
func TestAReplacementCannotInvertTheLevels(t *testing.T) {
	s := protectedSession(t, 18_900, 19_500)
	before := s.JournalLen()

	if err := s.ReplaceProtection(entryRef("entry"), 19_600, 19_500, decidedAt); !errors.Is(err, session.ErrProtectionInverted) {
		t.Fatalf("error: got %v, want %v", err, session.ErrProtectionInverted)
	}
	if s.JournalLen() != before || onlyPlanned(t, s).StopPrice != 18_900 {
		t.Fatal("a refused replacement changed the protection")
	}

	// Moving a single level past nothing is still allowed.
	if err := s.ReplaceProtection(entryRef("entry"), 19_400, 19_500, decidedAt); err != nil {
		t.Fatalf("a replacement the right way round was refused: %v", err)
	}
}

// Scenario: a name is never reused, and the system's namespace is its own
func TestIdentifiersAreSpentForever(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))

	// An order that finishes still owns its name.
	mustSubmit(t, s, order("used", market.SideBuy, 1))
	if err := s.SubmitOrder(order("used", market.SideSell, 1), decidedAt); !errors.Is(err, session.ErrOrderIDReused) {
		t.Fatalf("reusing a finished name: got %v, want %v", err, session.ErrOrderIDReused)
	}

	if err := s.SubmitOrder(limitOrder(t, "praxis:1:stop", market.SideBuy, 1, 19_000), decidedAt); !errors.Is(err, session.ErrReservedNamespace) {
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
			if err := s.ReplaceProtection(entryRef("entry"), 18_800, 0, decidedAt); err != nil {
				t.Fatalf("ReplaceProtection: %v", err)
			}
		}},
		{"after withdrawal", func(t *testing.T, s *session.Session) {
			if err := s.CancelProtection(entryRef("entry"), decidedAt); err != nil {
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
			if err := resumed.SubmitOrder(order("entry", market.SideBuy, 1), decidedAt); err == nil {
				t.Fatal("a resumed session reused a name")
			}
		})
	}
}

// Property: the same commands produce the same journal, every time.
func TestPropertyProtectionCommandsAreDeterministic(t *testing.T) {
	build := func() []session.Event {
		s := protectedSession(t, 18_900, 19_500)
		if err := s.ReplaceProtection(entryRef("entry"), 18_800, 19_600, decidedAt); err != nil {
			t.Fatalf("ReplaceProtection: %v", err)
		}
		if err := s.CancelProtection(entryRef("entry"), decidedAt); err != nil {
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
	if err := s.SubmitOrderWithProtection(limitOrder(t, "entry", market.SideBuy, 1, 19_000), 18_900, 0, decidedAt); !errors.Is(err, session.ErrNoSessionOpen) {
		t.Fatalf("error: got %v, want %v", err, session.ErrNoSessionOpen)
	}
	if err := s.CancelProtection(entryRef("entry"), decidedAt); !errors.Is(err, session.ErrNoSessionOpen) {
		t.Fatalf("error: got %v, want %v", err, session.ErrNoSessionOpen)
	}
}

// A reference naming a protection that does not exist is refused, whichever
// kind it is.
func TestAReferenceToNothingIsRefused(t *testing.T) {
	s := protectedSession(t, 18_900, 0)
	ref := session.ProtectionRef{Kind: session.ProtectionRefEpisode, EpisodeID: 7}
	if err := s.ReplaceProtection(ref, 18_800, 0, decidedAt); !errors.Is(err, session.ErrNoSuchProtection) {
		t.Fatalf("error: got %v, want %v", err, session.ErrNoSuchProtection)
	}
	if err := s.CancelProtection(entryRef("nobody"), decidedAt); !errors.Is(err, session.ErrNoSuchProtection) {
		t.Fatalf("error: got %v, want %v", err, session.ErrNoSuchProtection)
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
		if err := s.CancelProtection(ref, decidedAt); !errors.Is(err, session.ErrProtectionRef) {
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
		if err := s.ReplaceProtection(entryRef("entry"), 18_800, 19_500, decidedAt); err != nil {
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
	if err := s.ReplaceProtection(entryRef("entry"), 18_950, 19_500, decidedAt); err != nil {
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

// Scenario: a plan never outlives the entry it was placed with
//
//	Given a resting entry that carries a stop and a target
//	When the entry is cancelled
//	Then the cancellation and the ending of its plan are one batch, in that
//	  order, and nothing is left planned.
//
// A plan whose entry is gone names an order that no longer exists. It could
// never activate, and until this it was still reported as a protection —
// something the trader would read as cover they did not have.
func TestCancellingAProtectedEntryEndsItsPlan(t *testing.T) {
	s := protectedSession(t, 18_900, 19_500)
	before := s.JournalLen()

	if err := s.CancelOrder("entry", decidedAt); err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	if planned := s.PlannedProtections(); len(planned) != 0 {
		t.Fatalf("a plan outlived its entry: %+v", planned)
	}

	produced := s.Events()[before:]
	if got, want := kinds(produced), []session.Kind{
		session.KindOrderCancelled, session.KindProtectionEnded,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the batch is\n got: %v\nwant: %v", got, want)
	}

	cancelled, ok := produced[0].(session.OrderCancelled)
	if !ok || cancelled.Reason != session.CancelledByTrader {
		t.Fatalf("cancellation: got %+v", produced[0])
	}
	ended, ok := produced[1].(session.ProtectionEnded)
	if !ok {
		t.Fatalf("ending: got %+v", produced[1])
	}
	if ended.Reason != session.ProtectionEntryCancelled {
		t.Fatalf("reason: got %v, want %v", ended.Reason, session.ProtectionEntryCancelled)
	}
	if ended.Ref != entryRef("entry") {
		t.Fatalf("reference: got %+v, want the entry", ended.Ref)
	}
	// The levels it died holding are the ones it was last given.
	if ended.StopPrice != 18_900 || ended.TargetPrice != 19_500 {
		t.Fatalf("levels: got %d/%d, want 18900/19500", ended.StopPrice, ended.TargetPrice)
	}

	if err := session.Verify(s.Events()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	state, err := session.Replay(s.Events())
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(state.PlannedProtections) != 0 {
		t.Fatalf("a reconstructed plan outlived its entry: %+v", state.PlannedProtections)
	}
	resumed, err := session.Resume(state, nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(resumed.PlannedProtections()) != 0 {
		t.Fatal("a resumed session kept a plan whose entry was cancelled")
	}
}

// Scenario: a journal in which a plan outlived its entry is refused
//
// The rule is worth nothing if only the producer keeps it. Both the truncated
// case — the log simply ends — and the interrupted case — some other event
// arrives where the ending should be — are forgeries a reader must reject.
func TestAJournalCannotCancelAnEntryAndKeepItsPlan(t *testing.T) {
	build := func(t *testing.T) []session.Event {
		t.Helper()
		s := protectedSession(t, 18_900, 19_500)
		if err := s.CancelOrder("entry", decidedAt); err != nil {
			t.Fatalf("CancelOrder: %v", err)
		}
		return s.Events()
	}

	t.Run("the log ends there", func(t *testing.T) {
		events := build(t)
		if _, ok := events[len(events)-1].(session.ProtectionEnded); !ok {
			t.Fatalf("the last event is %T, not the ending this forges away", events[len(events)-1])
		}
		forged := events[:len(events)-1]

		if _, err := session.Replay(forged); !errors.Is(err, session.ErrFabricated) {
			t.Fatalf("Replay: got %v, want %v", err, session.ErrFabricated)
		}
		if err := session.Verify(forged); !errors.Is(err, session.ErrContradictoryLog) {
			t.Fatalf("Verify: got %v, want %v", err, session.ErrContradictoryLog)
		}
	})

	t.Run("something else stands in its place", func(t *testing.T) {
		events := build(t)
		last := len(events) - 1
		header := events[last].Header()
		// A structurally impeccable event, at the right sequence and time,
		// naming an order nothing is waiting for.
		events[last] = session.OrderCancelled{
			Envelope: session.Envelope{
				Time: header.Time, Sequence: header.Sequence, Kind: session.KindOrderCancelled,
			},
			OrderID: "somebody-else", RemainingQty: 1,
			Reason: session.CancelledByTrader, DecidedAt: decidedAt,
		}

		if _, err := session.Replay(events); !errors.Is(err, session.ErrFabricated) {
			t.Fatalf("Replay: got %v, want %v", err, session.ErrFabricated)
		}
		if err := session.Verify(events); !errors.Is(err, session.ErrContradictoryLog) {
			t.Fatalf("Verify: got %v, want %v", err, session.ErrContradictoryLog)
		}
	})
}

// twoCancelledEntries is a journal in which two protected entries are each
// cancelled, so that every ending exists somewhere in the log. It is the fixture
// the ordering forgeries need: a rule that only counted endings would accept
// all of them.
func twoCancelledEntries(t *testing.T) []session.Event {
	t.Helper()
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	for _, id := range []string{"a", "b"} {
		if err := s.SubmitOrderWithProtection(limitOrder(t, id, market.SideBuy, 2, 19_000), 18_900, 19_500, decidedAt); err != nil {
			t.Fatalf("SubmitOrderWithProtection %s: %v", id, err)
		}
	}
	for _, id := range []string{"a", "b"} {
		if err := s.CancelOrder(id, decidedAt); err != nil {
			t.Fatalf("CancelOrder %s: %v", id, err)
		}
	}
	if err := session.Verify(s.Events()); err != nil {
		t.Fatalf("the fixture does not verify: %v", err)
	}
	return s.Events()
}

// swapPayloads exchanges two events, leaving each envelope where it was, so the
// log stays contiguous in sequence and in time and only the order of the facts
// is forged.
func swapPayloads(t *testing.T, events []session.Event, i, j int) {
	t.Helper()
	restamp := func(e session.Event, at session.Envelope) session.Event {
		switch v := e.(type) {
		case session.OrderCancelled:
			v.Envelope = session.Envelope{Time: at.Time, Sequence: at.Sequence, Kind: session.KindOrderCancelled}
			return v
		case session.ProtectionEnded:
			v.Envelope = session.Envelope{Time: at.Time, Sequence: at.Sequence, Kind: session.KindProtectionEnded}
			return v
		default:
			t.Fatalf("this forgery does not know how to move a %T", e)
			return nil
		}
	}
	here, there := events[i].Header(), events[j].Header()
	events[i], events[j] = restamp(events[j], here), restamp(events[i], there)
}

func indexOfKind(t *testing.T, events []session.Event, k session.Kind, nth int) int {
	t.Helper()
	seen := 0
	for n, e := range events {
		if e.Header().Kind != k {
			continue
		}
		seen++
		if seen == nth {
			return n
		}
	}
	t.Fatalf("no %v number %d in the log", k, nth)
	return -1
}

// Scenario: an ending belongs to the cancellation it followed
//
// Counting endings is not enough. A log can hold every ending it owes and still
// be false about which cancellation each one answered, or let something else
// stand between the two — and a reader that only checked the totals would
// accept both. The cancellation and the ending are one decision, so the ending
// is the very next thing.
func TestAnEndingMustFollowTheCancellationItAnswers(t *testing.T) {
	tests := []struct {
		name  string
		forge func(*testing.T, []session.Event)
	}{
		{"an ending answering the wrong cancellation", func(t *testing.T, e []session.Event) {
			swapPayloads(t, e,
				indexOfKind(t, e, session.KindProtectionEnded, 1),
				indexOfKind(t, e, session.KindProtectionEnded, 2))
		}},
		{"a cancellation standing between the two", func(t *testing.T, e []session.Event) {
			swapPayloads(t, e,
				indexOfKind(t, e, session.KindProtectionEnded, 1),
				indexOfKind(t, e, session.KindOrderCancelled, 2))
		}},
		{"an ending that claims another reason", func(t *testing.T, e []session.Event) {
			at := indexOfKind(t, e, session.KindProtectionEnded, 1)
			ended := e[at].(session.ProtectionEnded)
			ended.Reason, ended.DecidedAt = session.ProtectionWithdrawnByTrader, decidedAt
			e[at] = ended
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events := twoCancelledEntries(t)
			tc.forge(t, events)

			if _, err := session.Replay(events); !errors.Is(err, session.ErrFabricated) {
				t.Fatalf("Replay: got %v, want %v", err, session.ErrFabricated)
			}
			if err := session.Verify(events); !errors.Is(err, session.ErrContradictoryLog) {
				t.Fatalf("Verify: got %v, want %v", err, session.ErrContradictoryLog)
			}
		})
	}
}

// Scenario: a protection is placed before the fill that will activate it
//
//	Given an entry submitted with levels that executes immediately
//	Then ProtectionPlaced stands between the decision and its first fill.
//
// Activation binds a plan to what the fill actually did. A plan recorded after
// the fill could not be bound to it: the episode would already have opened
// against a projection that had never heard of the plan, and the causal link
// would have to be inferred backwards from a later event.
func TestAProtectionIsPlacedBeforeItsFill(t *testing.T) {
	// Each case fills at once, and differs in what happens to what is left.
	tests := []struct {
		name  string
		entry func(*testing.T) market.Order
		size  market.Qty
		want  []session.Kind
	}{
		{
			name:  "a market entry, filled whole",
			entry: func(t *testing.T) market.Order { return order("entry", market.SideBuy, 2) },
			size:  50,
			want: []session.Kind{
				session.KindOrderSubmitted, session.KindProtectionPlaced,
				session.KindFillProduced, session.KindPositionChanged,
				session.KindAccountValued,
			},
		},
		{
			name: "a limit already executable",
			entry: func(t *testing.T) market.Order {
				return limitOrder(t, "entry", market.SideBuy, 2, 20_100)
			},
			size: 50,
			want: []session.Kind{
				session.KindOrderSubmitted, session.KindProtectionPlaced,
				session.KindFillProduced, session.KindPositionChanged,
				session.KindAccountValued,
			},
		},
		{
			name: "a limit the book fills in part",
			entry: func(t *testing.T) market.Order {
				return limitOrder(t, "entry", market.SideBuy, 5, 20_100)
			},
			size: 2,
			want: []session.Kind{
				session.KindOrderSubmitted, session.KindProtectionPlaced,
				session.KindFillProduced, session.KindPositionChanged,
				session.KindOrderRested, session.KindAccountValued,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newSession(t)
			mustOpen(t, s, 2_000, "d1")
			mustObserve(t, s, sized(3_000, 20_000, 20_001, tc.size))
			before := s.JournalLen()

			if err := s.SubmitOrderWithProtection(tc.entry(t), 19_900, 20_400, decidedAt); err != nil {
				t.Fatalf("SubmitOrderWithProtection: %v", err)
			}
			if got := kinds(s.Events()[before:]); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("the batch is\n got: %v\nwant: %v", got, tc.want)
			}
			if err := session.Verify(s.Events()); err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if _, err := session.Replay(s.Events()); err != nil {
				t.Fatalf("Replay: %v", err)
			}
		})
	}
}

// Scenario: an entry that could not fill at all takes its plan with it
//
//	Given a protected market entry meeting a book with nothing on it
//	Then the entry's whole remainder is cancelled and its plan ends with it.
//
// Cancelling the remainder is the other way an entry stops existing. The plan
// is asked of the projection rather than assumed, so once activation arrives a
// partly filled entry whose remainder is cancelled will keep protecting what
// it opened — this case is the one where nothing opened.
func TestAnEntryThatFillsNothingTakesItsPlanWithIt(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, market.Quote{
		Instrument: mnq, Time: 3_000,
		Bid: 20_000, Ask: 20_001, BidSize: 50, AskSize: 0,
	})
	before := s.JournalLen()

	if err := s.SubmitOrderWithProtection(order("entry", market.SideBuy, 2), 19_900, 20_400, decidedAt); err != nil {
		t.Fatalf("SubmitOrderWithProtection: %v", err)
	}

	if got, want := kinds(s.Events()[before:]), []session.Kind{
		session.KindOrderSubmitted, session.KindProtectionPlaced,
		session.KindOrderCancelled, session.KindProtectionEnded,
		session.KindAccountValued,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the batch is\n got: %v\nwant: %v", got, want)
	}
	if planned := s.PlannedProtections(); len(planned) != 0 {
		t.Fatalf("a plan outlived an entry that never existed in the book: %+v", planned)
	}
	if err := session.Verify(s.Events()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	state, err := session.Replay(s.Events())
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(state.PlannedProtections) != 0 {
		t.Fatalf("reconstructed a plan with no entry: %+v", state.PlannedProtections)
	}
}

// Scenario: a failure after the protection was recorded takes the whole batch
//
//	Given a protected entry whose fill the account cannot represent
//	Then the session stops, and the store holds every earlier command whole
//	  and nothing at all of this one.
//
// The protection is now recorded before the fill, so this branch runs with a
// ProtectionPlaced already in the journal. Half a batch reaching the store
// would leave a protection whose entry never happened.
func TestAFailureAfterTheProtectionLosesTheWholeBatch(t *testing.T) {
	committer := &failOnce{failAt: -1}
	s, err := session.New(config(), 1_000, committer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mustOpen(t, s, 2_000, "d1")
	huge := market.Quote{
		Instrument: mnq, Time: 3_000,
		Bid: 20_000, Ask: 20_001,
		BidSize: math.MaxInt64, AskSize: math.MaxInt64,
	}
	mustObserve(t, s, huge)
	committed := len(committer.batches)

	entry := order("entry", market.SideBuy, math.MaxInt64)
	err = s.SubmitOrderWithProtection(entry, 19_900, 20_400, decidedAt)
	if !errors.Is(err, session.ErrSessionNeedsRecovery) {
		t.Fatalf("error: got %v, want %v", err, session.ErrSessionNeedsRecovery)
	}
	if !errors.Is(err, market.ErrOverflow) {
		t.Fatalf("the cause is not reported: %v", err)
	}
	if len(committer.batches) != committed {
		t.Fatal("a batch reached the store from a command that failed")
	}
	// The point of the case: the journal had already spoken about the
	// protection when the account refused the fill.
	var recorded bool
	for _, e := range s.Events() {
		if _, ok := e.(session.ProtectionPlaced); ok {
			recorded = true
		}
	}
	if !recorded {
		t.Fatal("the failure happened before the protection was recorded, so this proves nothing")
	}

	// What the store does hold is a whole journal, with no entry and no
	// protection in it.
	var confirmed []session.Event
	for _, b := range committer.batches {
		confirmed = append(confirmed, b...)
	}
	state, err := session.Replay(confirmed)
	if err != nil {
		t.Fatalf("Replay of the confirmed batches: %v", err)
	}
	if len(state.PlannedProtections) != 0 {
		t.Fatalf("the store holds a protection from a command that failed: %+v", state.PlannedProtections)
	}
	// And the name is unspent: nothing was recorded, so nothing claimed it.
	resumed, err := session.Resume(state, committer)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := resumed.SubmitOrder(order("entry", market.SideBuy, 1), decidedAt); err != nil {
		t.Fatalf("the failed command spent its identifier: %v", err)
	}
}

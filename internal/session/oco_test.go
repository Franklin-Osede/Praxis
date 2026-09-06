package session_test

import (
	"errors"
	"reflect"
	"testing"

	"praxis/internal/market"
	"praxis/internal/session"
)

// protectedLong is a long of ten contracts with a stop at 19,900 and a target
// at 20,400, activated by the fill that opened it.
func protectedLong(t *testing.T) *session.Session {
	t.Helper()
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	mustProtect(t, s, order("entry", market.SideBuy, 10), 19_900, 20_400)
	if got := onlyActive(t, s).ProtectedQty; got != 10 {
		t.Fatalf("the fixture covers %d contracts, want 10", got)
	}
	return s
}

// legsOf reads the names a protection's levels were given, from the journal
// rather than from a guess about how many events came before them.
func legsOf(t *testing.T, s *session.Session) (stop, target string) {
	t.Helper()
	for _, e := range s.Events() {
		if v, ok := e.(session.ProtectionPlaced); ok {
			return v.StopOrderID, v.TargetOrderID
		}
	}
	t.Fatal("the journal placed no protection")
	return "", ""
}

func netQty(t *testing.T, s *session.Session) market.Qty {
	t.Helper()
	position, _ := s.Account().Position(mnq)
	return position.NetQty
}

// cancellations lists the order cancellations a journal holds, as identifier,
// remaining quantity and reason.
type cancellation struct {
	OrderID   string
	Remaining market.Qty
	Reason    string
}

func cancellations(events []session.Event) []cancellation {
	var out []cancellation
	for _, e := range events {
		if v, ok := e.(session.OrderCancelled); ok {
			out = append(out, cancellation{v.OrderID, v.RemainingQty, v.Reason.String()})
		}
	}
	return out
}

// Scenario: a protection meets the observation that activated it
//
//	Given a market entry submitted with a stop the market has already passed
//	When it fills
//	Then the stop executes on that same observation.
//
// Waiting for the next one would grant a survival the market never gave: the
// price was already through the level when the position opened. The entry buys
// against the ask and the stop sells against the bid, so the two draw on
// different sides of the book and neither takes the other's liquidity.
func TestAStopAlreadyPassedExecutesOnTheObservationThatActivatedIt(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	before := s.JournalLen()

	// The stop sits above the bid: the entry fills at 20,001 and the position
	// is already through 20,050 the moment it exists.
	mustProtect(t, s, order("entry", market.SideBuy, 4), 20_050, 20_400)

	if got, want := kinds(s.Events()[before:]), []session.Kind{
		session.KindOrderSubmitted, session.KindProtectionPlaced,
		session.KindFillProduced, session.KindPositionChanged,
		// The same observation, still: the stop fills against the bid.
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
	// The stop sold at the bid, not at its own level: a stop that is already
	// through fills where the market is, which is worse for the trader.
	var stopFill market.Fill
	for _, e := range s.Events() {
		if v, ok := e.(session.FillProduced); ok && v.Fill.Side == market.SideSell {
			stopFill = v.Fill
		}
	}
	if stopFill.Price != 20_000 {
		t.Fatalf("the stop filled at %d, want the bid of 20000", stopFill.Price)
	}
	checked(t, s)
}

// The same for a target the observation has already reached.
func TestATargetAlreadyReachedExecutesOnTheSameObservation(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	mustProtect(t, s, order("entry", market.SideBuy, 4), 19_900, 19_950)

	if net := netQty(t, s); net != 0 {
		t.Fatalf("position: got %d, want the target to have closed it", net)
	}
	// A limit fills at its own price and never better, even where the book was
	// better: the trader is given what they asked for, not what the market
	// happened to offer, because the engine cannot know they would have been
	// first in the queue for it.
	var exit market.Fill
	for _, e := range s.Events() {
		if v, ok := e.(session.FillProduced); ok && v.Fill.Side == market.SideSell {
			exit = v.Fill
		}
	}
	if exit.Price != 19_950 {
		t.Fatalf("the target filled at %d, want its own price of 19950", exit.Price)
	}
	checked(t, s)
}

// Scenario: a target that fills in part keeps both legs over what is left
func TestAPartialTargetKeepsBothLegs(t *testing.T) {
	s := protectedLong(t)
	// The bid is through the target, and there are three contracts on it.
	mustObserve(t, s, sized(4_000, 20_500, 20_501, 3))

	active := onlyActive(t, s)
	if active.StopPrice != 19_900 || active.TargetPrice != 20_400 {
		t.Fatalf("levels: got %+v, want both legs standing", active)
	}
	if active.ProtectedQty != 7 || netQty(t, s) != 7 {
		t.Fatalf("cover %d over a position of %d, want 7 and 7", active.ProtectedQty, netQty(t, s))
	}
	if got := cancellations(s.Events()); got != nil {
		t.Fatalf("a partial target cancelled something: %v", got)
	}
	covered(t, s)
	checked(t, s)
}

// Scenario: a stop that fills in part takes only itself away
//
// A stop that reached its level has triggered and cannot untrigger. What the
// book could not fill does not go back to waiting, so the stop ends — and the
// target survives over the seven contracts still open, which is a position with
// a target and no stop, and is exactly why Executed is a reason rather than a
// state.
func TestAPartialStopEndsOnlyTheStop(t *testing.T) {
	s := protectedLong(t)
	mustObserve(t, s, sized(4_000, 19_800, 19_801, 3))

	active := onlyActive(t, s)
	if active.StopPrice != 0 || active.StopOrderID != "" {
		t.Fatalf("the stop survived: %+v", active)
	}
	if active.TargetPrice != 20_400 {
		t.Fatalf("the target went with it: %+v", active)
	}
	if active.ProtectedQty != 7 || netQty(t, s) != 7 {
		t.Fatalf("cover %d over a position of %d, want 7 and 7", active.ProtectedQty, netQty(t, s))
	}
	stopLeg, _ := legsOf(t, s)
	if got, want := cancellations(s.Events()), []cancellation{
		{stopLeg, 7, "unfillable remainder"},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cancellations\n got: %v\nwant: %v", got, want)
	}
	covered(t, s)
	checked(t, s)
}

// Scenario: a stop through its level with nothing on the book still ends
//
// It triggered. There is no position change to derive that from, so the
// cancellation is the only record of it — and the position is untouched, which
// is the thing a trader would least expect and most needs written down.
func TestAStopWithNoLiquidityEndsWithoutTouchingThePosition(t *testing.T) {
	s := protectedLong(t)
	mustObserve(t, s, market.Quote{
		Instrument: mnq, Time: 4_000, Bid: 19_800, Ask: 19_801, BidSize: 0, AskSize: 50,
	})

	if net := netQty(t, s); net != 10 {
		t.Fatalf("position: got %d, want the 10 untouched", net)
	}
	active := onlyActive(t, s)
	if active.StopPrice != 0 || active.TargetPrice != 20_400 || active.ProtectedQty != 10 {
		t.Fatalf("protection: got %+v, want the target alone over 10", active)
	}
	stopLeg, _ := legsOf(t, s)
	if got, want := cancellations(s.Events()), []cancellation{
		{stopLeg, 10, "unfillable remainder"},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cancellations\n got: %v\nwant: %v", got, want)
	}
	covered(t, s)
	checked(t, s)
}

// Scenario: a leg that closes the position cancels its sibling
func TestALegThatClosesThePositionCancelsItsSibling(t *testing.T) {
	tests := []struct {
		name     string
		quote    market.Quote
		stopGoes bool
	}{
		{"the target executes", sized(4_000, 20_500, 20_501, 50), true},
		{"the stop executes", sized(4_000, 19_800, 19_801, 50), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := protectedLong(t)
			stopLeg, targetLeg := legsOf(t, s)
			sibling, executed := targetLeg, stopLeg
			if tc.stopGoes {
				sibling, executed = stopLeg, targetLeg
			}
			mustObserve(t, s, tc.quote)

			if net := netQty(t, s); net != 0 {
				t.Fatalf("position: got %d, want flat", net)
			}
			if got, want := cancellations(s.Events()), []cancellation{
				{sibling, 10, "by one-cancels-the-other"},
			}; !reflect.DeepEqual(got, want) {
				t.Fatalf("cancellations\n got: %v\nwant: %v", got, want)
			}
			// The leg that executed is not cancelled: it filled the position
			// away, and there is nothing of it left to withdraw.
			for _, c := range cancellations(s.Events()) {
				if c.OrderID == executed {
					t.Fatal("the leg that executed was also cancelled")
				}
			}
			if got, want := endings(s.Events()), [][2]string{
				{"episode:" + itoa(episodeOf(t, s)), "executed"},
			}; !reflect.DeepEqual(got, want) {
				t.Fatalf("endings\n got: %v\nwant: %v", got, want)
			}
			checked(t, s)
		})
	}
}

// episodeOf is the identity of the episode the fixture opened, read from the
// journal rather than guessed at.
func episodeOf(t *testing.T, s *session.Session) uint64 {
	t.Helper()
	for _, e := range s.Events() {
		if v, ok := e.(session.PositionChanged); ok {
			return v.Sequence
		}
	}
	t.Fatal("the journal opened no episode")
	return 0
}

// Scenario: a manual exit says something different from an execution
//
// Both end the protection and both take the legs with them, but the observable
// cause is not the same. A log that spelled them alike could not tell a stop
// that worked from one the trader overtook, which is a behavioural difference
// and the whole reason the log exists.
func TestAManualExitDoesNotClaimOneCancelledTheOther(t *testing.T) {
	s := protectedLong(t)
	mustSubmit(t, s, order("exit", market.SideSell, 10))

	stopLeg, targetLeg := legsOf(t, s)
	if got, want := cancellations(s.Events()), []cancellation{
		{stopLeg, 10, "position closed"},
		{targetLeg, 10, "position closed"},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cancellations\n got: %v\nwant: %v", got, want)
	}
	if got, want := endings(s.Events()), [][2]string{
		{"episode:" + itoa(episodeOf(t, s)), "position_closed"},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("endings\n got: %v\nwant: %v", got, want)
	}
	checked(t, s)
}

// Scenario: a protection takes the liquidity before the next working order
//
//	Given a resting entry that will fill and activate a stop already passed
//	And a second resting order behind it that wants the same side of the book
//	When one observation reaches all of them
//	Then the stop is filled before the second order is offered anything.
//
// Leaving every protection to the end of the observation would let the second
// order take the contracts the stop should have found. Without a real queue
// position there is nothing in the data that says otherwise, and giving the
// protection priority is the conservative reading.
func TestAProtectionTakesLiquidityBeforeTheNextWorkingOrder(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))

	// A buy limit that will fill low, carrying a stop just under where it
	// fills, and behind it a stop of the trader's own at the same level,
	// wanting the same side of the same book.
	mustProtect(t, s, limitOrder(t, "entry", market.SideBuy, 4, 19_500), 19_450, 20_400)
	mustSubmit(t, s, stopOrder(t, "behind", market.SideSell, 4, 19_450))

	// One observation reaches all three, and the bid shows four contracts.
	mustObserve(t, s, sized(4_000, 19_400, 19_401, 4))

	if net := netQty(t, s); net != 0 {
		t.Fatalf("position: got %d, want the protection to have closed the entry", net)
	}
	// The protection took the whole bid, so the order behind it found nothing
	// — and having triggered, it does not go back to waiting.
	var behindFilled market.Qty
	for _, e := range s.Events() {
		if v, ok := e.(session.FillProduced); ok && v.Fill.OrderID == "behind" {
			behindFilled += v.Fill.Qty
		}
	}
	if behindFilled != 0 {
		t.Fatalf("the order behind took %d contracts the protection should have found", behindFilled)
	}
	if len(s.WorkingOrders()) != 0 {
		t.Fatalf("working: got %+v, want the triggered stop gone", s.WorkingOrders())
	}
	checked(t, s)
}

// Scenario: a protection survives a journal in each state execution leaves it
func TestProtectionReconstructsAfterExecution(t *testing.T) {
	tests := []struct {
		name  string
		quote market.Quote
	}{
		{"both legs, untouched", sized(4_000, 20_000, 20_001, 50)},
		{"the target alone, after a partial stop", sized(4_000, 19_800, 19_801, 3)},
		{"the target alone, after a stop with nothing behind it", market.Quote{
			Instrument: mnq, Time: 4_000, Bid: 19_800, Ask: 19_801, BidSize: 0, AskSize: 50,
		}},
		{"ended, both legs, by execution", sized(4_000, 20_500, 20_501, 50)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := protectedLong(t)
			mustObserve(t, s, tc.quote)
			checked(t, s)
		})
	}
}

// Scenario: a store that fails during a protective batch loses all of it
//
// The batch now holds an entry, a protection, two fills, two position changes,
// a cancellation and an ending. Half of it on disk would be a journal claiming
// a protection over a position that never opened.
func TestAProtectiveBatchIsAllOrNothing(t *testing.T) {
	committer := &failOnce{failAt: 4}
	s, err := session.New(config(), 1_000, committer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	committed := len(committer.batches)

	// The command that fails is the protected entry whose stop is already
	// through: one batch, nine events, none of which may survive alone.
	err = s.SubmitOrderWithProtection(order("entry", market.SideBuy, 4), 20_050, 20_400, decidedAt)
	if !errors.Is(err, session.ErrSessionNeedsRecovery) {
		t.Fatalf("error: got %v, want %v", err, session.ErrSessionNeedsRecovery)
	}
	if len(committer.batches) != committed {
		t.Fatal("a batch reached the store from a command that failed")
	}
	if len(s.Events()) <= s.JournalLen()-9 {
		t.Fatal("the fixture produced fewer events than the case is about")
	}

	var confirmed []session.Event
	for _, b := range committer.batches {
		confirmed = append(confirmed, b...)
	}
	state, err := session.Replay(confirmed)
	if err != nil {
		t.Fatalf("Replay of the confirmed batches: %v", err)
	}
	if len(state.ActiveProtections) != 0 || len(state.PlannedProtections) != 0 {
		t.Fatalf("the store holds protection from a command that failed: %+v %+v",
			state.ActiveProtections, state.PlannedProtections)
	}
	if state.Account.RealisedCts() != 0 {
		t.Fatalf("the store holds money from a command that failed: %d", state.Account.RealisedCts())
	}
}

// Property: no observation makes both legs reachable at once
//
// The stop of a long sells at or below its level and the target sells at or
// above a higher one, so a single bid cannot satisfy both; a short is the same
// against the ask. This is why the geometry rule earns its place, and it is
// also why "the stop wins when both are reachable" has no test yet: with a
// two-sided quote the situation cannot be constructed at all.
//
// The order is fixed in the code regardless, because a bar carries a high and a
// low and will make it constructible. This property is what would break first
// if the geometry rule were ever relaxed, which is the failure that would let
// the untested priority start to matter.
func TestPropertyAQuoteNeverReachesBothLegs(t *testing.T) {
	for bid := market.Ticks(19_700); bid <= 20_600; bid += 25 {
		s := protectedLong(t)
		mustObserve(t, s, sized(4_000, bid, bid+1, 50))

		var exits int
		for _, e := range s.Events() {
			if v, ok := e.(session.FillProduced); ok && v.Fill.Side == market.SideSell {
				exits++
			}
		}
		if exits > 1 {
			t.Fatalf("a bid of %d filled %d protective legs", bid, exits)
		}
		// And whichever it was, the log still adds up.
		if err := session.Verify(s.Events()); err != nil {
			t.Fatalf("bid %d: Verify: %v", bid, err)
		}
	}
}

// Scenario: the tail a protective execution leaves cannot be forged
//
// The sibling's cancellation and the aggregate's ending are derived facts: what
// was cancelled, how much of it was left, why, and what the protection was
// holding when it ended are all recomputable from the events that caused them.
// A journal that says any of it differently is refused rather than believed.
func TestAProtectiveTailCannotBeForged(t *testing.T) {
	build := func(t *testing.T) []session.Event {
		t.Helper()
		s := protectedLong(t)
		mustObserve(t, s, sized(4_000, 20_500, 20_501, 50))
		if err := session.Verify(s.Events()); err != nil {
			t.Fatalf("the fixture does not verify: %v", err)
		}
		return s.Events()
	}

	tests := []struct {
		name  string
		forge func(*testing.T, []session.Event)
	}{
		{"the wrong order was cancelled", func(t *testing.T, e []session.Event) {
			at := indexOfKind(t, e, session.KindOrderCancelled, 1)
			c := e[at].(session.OrderCancelled)
			c.OrderID = "entry"
			e[at] = c
		}},
		{"a different quantity was left on it", func(t *testing.T, e []session.Event) {
			at := indexOfKind(t, e, session.KindOrderCancelled, 1)
			c := e[at].(session.OrderCancelled)
			c.RemainingQty = 3
			e[at] = c
		}},
		{"the trader is blamed for it", func(t *testing.T, e []session.Event) {
			at := indexOfKind(t, e, session.KindOrderCancelled, 1)
			c := e[at].(session.OrderCancelled)
			c.Reason, c.Decided = session.CancelledByTrader, decidedAt
			e[at] = c
		}},
		{"the ending claims other levels", func(t *testing.T, e []session.Event) {
			at := indexOfKind(t, e, session.KindProtectionEnded, 1)
			v := e[at].(session.ProtectionEnded)
			v.StopPrice = 19_000
			e[at] = v
		}},
		{"the ending claims another reason", func(t *testing.T, e []session.Event) {
			at := indexOfKind(t, e, session.KindProtectionEnded, 1)
			v := e[at].(session.ProtectionEnded)
			v.Reason = session.ProtectionPositionClosed
			e[at] = v
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events := build(t)
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

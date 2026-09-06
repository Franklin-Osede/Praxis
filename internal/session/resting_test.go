package session_test

import (
	"errors"
	"reflect"
	"testing"

	"praxis/internal/market"
	"praxis/internal/session"
)

func limitOrder(t *testing.T, id string, side market.Side, qty market.Qty, limit market.Ticks) market.Order {
	t.Helper()
	o, err := market.NewLimitOrder(id, mnq, side, qty, limit)
	if err != nil {
		t.Fatalf("NewLimitOrder: %v", err)
	}
	return o
}

func stopOrder(t *testing.T, id string, side market.Side, qty market.Qty, stop market.Ticks) market.Order {
	t.Helper()
	o, err := market.NewStopOrder(id, mnq, side, qty, stop)
	if err != nil {
		t.Fatalf("NewStopOrder: %v", err)
	}
	return o
}

func sized(at market.LogicalTime, bid, ask market.Ticks, size market.Qty) market.Quote {
	return market.Quote{Instrument: mnq, Time: at, Bid: bid, Ask: ask, BidSize: size, AskSize: size}
}

// Scenario: a stop waits until an observation reaches it
//
//	Given a sell stop placed below the market
//	When it is submitted, nothing fills and the order rests
//	And when a later observation's bid reaches its level, it fills there —
//	  at that observation's time, not at the one it was submitted on.
//
// A stop is only a stop because it survives. Until it could, the log recorded a
// protective decision the engine did not honour.
func TestAStopRestsUntilAnObservationReachesIt(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 10))
	mustSubmit(t, s, order("entry", market.SideBuy, 2))

	mustSubmit(t, s, stopOrder(t, "stop", market.SideSell, 2, 19_990))
	if working := s.WorkingOrders(); len(working) != 1 || working[0].ID != "stop" {
		t.Fatalf("working: got %+v, want the stop waiting", working)
	}
	if p, _ := s.Account().Position(mnq); p.NetQty != 2 {
		t.Fatalf("position: got %d, want the stop not to have fired", p.NetQty)
	}

	// An observation that does not reach it changes nothing.
	mustObserve(t, s, sized(4_000, 19_995, 19_996, 10))
	if len(s.WorkingOrders()) != 1 {
		t.Fatal("the stop stopped waiting without being reached")
	}

	mustObserve(t, s, sized(5_000, 19_990, 19_991, 10))
	if len(s.WorkingOrders()) != 0 {
		t.Fatalf("working: got %+v, want the stop gone", s.WorkingOrders())
	}
	if p, _ := s.Account().Position(mnq); !p.IsFlat() {
		t.Fatalf("position: got %d, want flat", p.NetQty)
	}

	var fill session.FillProduced
	for _, e := range s.Events() {
		if f, ok := e.(session.FillProduced); ok && f.Fill.OrderID == "stop" {
			fill = f
		}
	}
	if fill.Fill.Qty != 2 || fill.Fill.Price != 19_990 || fill.Fill.Time != 5_000 {
		t.Fatalf("fill: got %+v, want 2 at 19990 on the observation that reached it", fill.Fill)
	}
}

// Scenario: what the book could not fill is named, not dropped
//
//	Given a market order larger than the size displayed
//	When it executes
//	Then the remainder is cancelled and the log says so, because a market
//	  order does not rest: resting one would invent a price the trader never
//	  named.
//
// Before this, seven of ten contracts vanished with no event. "How often does
// the trader re-enter after a partial fill" would have measured the simulator.
func TestAMarketOrdersRemainderIsCancelledAndRecorded(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 3))

	mustSubmit(t, s, order("o-1", market.SideBuy, 10))

	if len(s.WorkingOrders()) != 0 {
		t.Fatalf("a market order rested: %+v", s.WorkingOrders())
	}
	var cancelled session.OrderCancelled
	for _, e := range s.Events() {
		if c, ok := e.(session.OrderCancelled); ok {
			cancelled = c
		}
	}
	if cancelled.OrderID != "o-1" || cancelled.RemainingQty != 7 {
		t.Fatalf("cancellation: got %+v, want seven contracts named", cancelled)
	}
	if cancelled.Reason != session.CancelledUnfillableRemainder {
		t.Fatalf("reason: got %v, want the unfillable remainder", cancelled.Reason)
	}
	if p, _ := s.Account().Position(mnq); p.NetQty != 3 {
		t.Fatalf("position: got %d, want the three the book showed", p.NetQty)
	}
}

// Scenario: a limit's remainder waits rather than being cancelled
func TestALimitsRemainderRests(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 3))

	mustSubmit(t, s, limitOrder(t, "o-1", market.SideBuy, 10, 20_005))

	working := s.WorkingOrders()
	if len(working) != 1 || working[0].Qty != 7 {
		t.Fatalf("working: got %+v, want seven still waiting", working)
	}

	// A later observation with size fills the rest, at the limit and not at
	// the touch.
	mustObserve(t, s, sized(4_000, 19_990, 19_991, 20))
	if len(s.WorkingOrders()) != 0 {
		t.Fatalf("working: got %+v, want it filled", s.WorkingOrders())
	}
	p, _ := s.Account().Position(mnq)
	if p.NetQty != 10 {
		t.Fatalf("position: got %d, want ten", p.NetQty)
	}
}

// Scenario: a trader can withdraw a waiting order
func TestCancellingAWorkingOrder(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 10))
	mustSubmit(t, s, order("entry", market.SideBuy, 2))
	mustSubmit(t, s, stopOrder(t, "stop", market.SideSell, 2, 19_990))

	if err := s.CancelOrder("stop", decidedAt()); err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	if len(s.WorkingOrders()) != 0 {
		t.Fatal("the order is still working")
	}

	// The level is no longer honoured, which is the point of cancelling it.
	mustObserve(t, s, sized(4_000, 19_980, 19_981, 10))
	if p, _ := s.Account().Position(mnq); p.NetQty != 2 {
		t.Fatalf("position: got %d, want a cancelled stop not to fire", p.NetQty)
	}

	var cancelled session.OrderCancelled
	for _, e := range s.Events() {
		if c, ok := e.(session.OrderCancelled); ok {
			cancelled = c
		}
	}
	if cancelled.Reason != session.CancelledByTrader || cancelled.RemainingQty != 2 {
		t.Fatalf("cancellation: got %+v", cancelled)
	}

	if err := s.CancelOrder("stop", decidedAt()); !errors.Is(err, session.ErrNoSuchOrder) {
		t.Fatalf("cancelling twice: got %v, want %v", err, session.ErrNoSuchOrder)
	}
}

// Two working orders cannot share an identifier, or a fill could not be
// attributed to the decision that produced it.
func TestAWorkingIdentifierCannotBeReused(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 10))
	mustSubmit(t, s, limitOrder(t, "o-1", market.SideBuy, 2, 19_000))

	if err := s.SubmitOrder(limitOrder(t, "o-1", market.SideBuy, 1, 19_000), decidedAt()); !errors.Is(err, session.ErrDuplicateOrderID) {
		t.Fatalf("error: got %v, want %v", err, session.ErrDuplicateOrderID)
	}
	if len(s.WorkingOrders()) != 1 {
		t.Fatal("a rejected duplicate changed the working set")
	}
}

// Scenario: waiting orders are offered an observation in the order they were
// submitted, and a scarce book goes to the first
func TestAScarceBookGoesToTheOrderThatWaitedLongest(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 10))
	mustSubmit(t, s, limitOrder(t, "first", market.SideBuy, 3, 19_500))
	mustSubmit(t, s, limitOrder(t, "second", market.SideBuy, 3, 19_500))

	// Only two contracts are offered to both.
	mustObserve(t, s, sized(4_000, 19_499, 19_500, 2))

	working := s.WorkingOrders()
	if len(working) != 2 {
		t.Fatalf("working: got %+v, want both still waiting", working)
	}
	if working[0].ID != "first" || working[0].Qty != 1 {
		t.Fatalf("first: got %+v, want one left of three", working[0])
	}
	if working[1].ID != "second" || working[1].Qty != 3 {
		t.Fatalf("second: got %+v, want it untouched", working[1])
	}
}

// Scenario: a working order survives a journal and comes back
func TestWorkingOrdersSurviveReplayAndResume(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 10))
	mustSubmit(t, s, order("entry", market.SideBuy, 2))
	mustSubmit(t, s, stopOrder(t, "stop", market.SideSell, 2, 19_990))
	mustSubmit(t, s, limitOrder(t, "target", market.SideSell, 2, 20_100))

	state, err := session.Replay(s.Events())
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if !reflect.DeepEqual(state.Working, s.WorkingOrders()) {
		t.Fatalf("working\n got: %+v\nwant: %+v", state.Working, s.WorkingOrders())
	}

	resumed, err := session.Resume(state, nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !reflect.DeepEqual(resumed.WorkingOrders(), s.WorkingOrders()) {
		t.Fatal("a resumed session forgot what was waiting")
	}

	// And the resumed session honours them, which is the whole point of
	// carrying them across.
	mustObserve(t, resumed, sized(5_000, 19_990, 19_991, 10))
	if p, _ := resumed.Account().Position(mnq); !p.IsFlat() {
		t.Fatalf("position: got %d, want the resumed stop to have fired", p.NetQty)
	}
}

// A partially filled order that later fills the rest leaves nothing working,
// and replay agrees.
func TestAPartiallyFilledRestingOrderReconstructs(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 3))
	mustSubmit(t, s, limitOrder(t, "o-1", market.SideBuy, 10, 20_005))
	mustObserve(t, s, sized(4_000, 19_990, 19_991, 4))

	if working := s.WorkingOrders(); len(working) != 1 || working[0].Qty != 3 {
		t.Fatalf("working: got %+v, want three left", working)
	}

	state, err := session.Replay(s.Events())
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if !reflect.DeepEqual(state.Working, s.WorkingOrders()) {
		t.Fatalf("working\n got: %+v\nwant: %+v", state.Working, s.WorkingOrders())
	}
	if err := session.Verify(s.Events()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// Scenario: the valuation an observation records already contains what that
// observation caused
//
//	Given a stop that fires on an observation
//	When the account is valued for that same observation
//	Then the valuation already holds the realised money, because revaluing
//	  first would report an equity that had not yet felt the fill — and an
//	  evaluation reading it would notice a breach one observation late.
func TestTheValuationContainsTheFillTheSameObservationCaused(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 10))
	mustSubmit(t, s, order("entry", market.SideBuy, 2))
	mustSubmit(t, s, stopOrder(t, "stop", market.SideSell, 2, 19_900))

	balanceBefore := lastValuation(t, s).BalanceCts
	mustObserve(t, s, sized(4_000, 19_900, 19_901, 10))

	if p, _ := s.Account().Position(mnq); !p.IsFlat() {
		t.Fatalf("the stop did not fire: position %d", p.NetQty)
	}

	valued := lastValuation(t, s)
	if valued.Time != 4_000 {
		t.Fatalf("the last valuation is from %d, want the observation that fired the stop", valued.Time)
	}
	if valued.BalanceCts >= balanceBefore {
		t.Fatalf("balance: got %d, want it below %d — the loss the stop realised",
			valued.BalanceCts, balanceBefore)
	}
	if valued.EquityCts != valued.BalanceCts {
		t.Fatalf("equity %d and balance %d differ with a flat position",
			valued.EquityCts, valued.BalanceCts)
	}
}

// Scenario: a stop that has triggered cannot untrigger
//
//	Given a sell stop for ten with only three contracts bid at its level
//	When the level is reached, three fill and the other seven are cancelled
//	And when the price comes back above the level, nothing more happens.
//
// A stop that reached its level has become a market order, and a market order
// does not wait. Leaving the remainder working would let it untrigger, and the
// trader would be protected by an instruction the market had already passed.
func TestATriggeredStopDoesNotUntrigger(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	mustSubmit(t, s, order("entry", market.SideBuy, 10))
	mustSubmit(t, s, stopOrder(t, "stop", market.SideSell, 10, 19_900))

	// The level is reached, and the book shows three.
	mustObserve(t, s, sized(4_000, 19_890, 19_891, 3))

	if working := s.WorkingOrders(); len(working) != 0 {
		t.Fatalf("working: got %+v, want a triggered stop gone", working)
	}
	if p, _ := s.Account().Position(mnq); p.NetQty != 7 {
		t.Fatalf("position: got %d, want seven left unprotected", p.NetQty)
	}

	var cancelled session.OrderCancelled
	for _, e := range s.Events() {
		if c, ok := e.(session.OrderCancelled); ok && c.OrderID == "stop" {
			cancelled = c
		}
	}
	if cancelled.RemainingQty != 7 || cancelled.Reason != session.CancelledUnfillableRemainder {
		t.Fatalf("cancellation: got %+v, want seven named as unfillable", cancelled)
	}

	// The price comes back. Nothing more may happen: the stop is gone.
	before := s.JournalLen()
	mustObserve(t, s, sized(5_000, 19_950, 19_951, 50))
	for _, e := range s.Events()[before:] {
		if f, ok := e.(session.FillProduced); ok && f.Fill.OrderID == "stop" {
			t.Fatal("a cancelled stop filled again")
		}
	}
	if p, _ := s.Account().Position(mnq); p.NetQty != 7 {
		t.Fatalf("position: got %d, want it unchanged", p.NetQty)
	}
}

// Scenario: a stop reaching its level with no liquidity is still triggered
//
//	Given a sell stop whose level is reached on an observation showing no
//	  bid size
//	Then nothing fills and the whole order is cancelled, because it
//	  triggered.
//
// An empty slice of fills means two different things — never reached, and
// reached with nothing to trade against — and only the second is irreversible.
func TestAStopTriggeredWithNoLiquidityIsCancelledWhole(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	mustSubmit(t, s, order("entry", market.SideBuy, 4))
	mustSubmit(t, s, stopOrder(t, "stop", market.SideSell, 4, 19_900))

	mustObserve(t, s, market.Quote{
		Instrument: mnq, Time: 4_000,
		Bid: 19_890, Ask: 19_891, BidSize: 0, AskSize: 50,
	})

	if working := s.WorkingOrders(); len(working) != 0 {
		t.Fatalf("working: got %+v, want the stop gone", working)
	}
	if p, _ := s.Account().Position(mnq); p.NetQty != 4 {
		t.Fatalf("position: got %d, want nothing to have filled", p.NetQty)
	}

	var cancelled session.OrderCancelled
	for _, e := range s.Events() {
		if c, ok := e.(session.OrderCancelled); ok && c.OrderID == "stop" {
			cancelled = c
		}
	}
	if cancelled.RemainingQty != 4 {
		t.Fatalf("cancellation: got %+v, want the whole order named", cancelled)
	}
}

// A stop that has not reached its level keeps waiting, which is the case the
// triggered rule must not swallow.
func TestAnUntriggeredStopKeepsWaiting(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	mustSubmit(t, s, order("entry", market.SideBuy, 4))
	mustSubmit(t, s, stopOrder(t, "stop", market.SideSell, 4, 19_900))

	// Nowhere near it, and with no size either.
	mustObserve(t, s, market.Quote{
		Instrument: mnq, Time: 4_000,
		Bid: 19_990, Ask: 19_991, BidSize: 0, AskSize: 0,
	})

	if working := s.WorkingOrders(); len(working) != 1 || working[0].Qty != 4 {
		t.Fatalf("working: got %+v, want it still waiting in full", working)
	}
}

// The fill and the cancellation both survive the journal.
func TestATriggeredStopReconstructs(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	mustSubmit(t, s, order("entry", market.SideBuy, 10))
	mustSubmit(t, s, stopOrder(t, "stop", market.SideSell, 10, 19_900))
	mustObserve(t, s, sized(4_000, 19_890, 19_891, 3))

	state, err := session.Replay(s.Events())
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(state.Working) != 0 {
		t.Fatalf("working: got %+v, want nothing waiting", state.Working)
	}
	if err := session.Verify(s.Events()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !reflect.DeepEqual(state.Account.Positions(), s.Account().Positions()) {
		t.Fatal("the reconstructed account differs")
	}
}

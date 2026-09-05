package session_test

import (
	"errors"
	"reflect"
	"testing"

	"praxis/internal/market"
	"praxis/internal/session"
)

// Scenario: a resumed session inherits the book as it was left, not as it
//
//	  arrived
//
//		Given an observation whose displayed size has been partly taken
//		When the session is rebuilt from its journal and carries on
//		Then the next order finds what is left, not what was there to begin with.
//
// One observation shows a finite book, and everything executing against it
// consumes what it takes. A reconstruction that started from the untouched
// quote would hand the resumed session liquidity the interrupted one had
// already spent — the same contracts sold twice, which is the largest way a
// simulator can invent depth.
func TestAResumedSessionInheritsTheBookAsItWasLeft(t *testing.T) {
	build := func(t *testing.T) *session.Session {
		t.Helper()
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		mustObserve(t, s, sized(3_000, 20_000, 20_001, 5))
		mustSubmit(t, s, order("first", market.SideBuy, 3))
		return s
	}

	// The uninterrupted run: a second order finds the two contracts left.
	continuous := build(t)
	mustSubmit(t, continuous, order("second", market.SideBuy, 4))

	interrupted := build(t)
	state, err := session.Replay(interrupted.Events())
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	resumed, err := session.Resume(state, nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	mustSubmit(t, resumed, order("second", market.SideBuy, 4))

	if !reflect.DeepEqual(resumed.Events(), continuous.Events()) {
		t.Fatalf("a resumed session traded a different book\n got: %v\nwant: %v",
			kinds(resumed.Events()), kinds(continuous.Events()))
	}
	position, _ := resumed.Account().Position(mnq)
	if position.NetQty != 5 {
		t.Fatalf("position: got %d, want the 5 the book actually showed", position.NetQty)
	}
}

// emptyBid is an observation through a long's stop with nothing on the bid.
func emptyBid(at market.LogicalTime, bid market.Ticks) market.Quote {
	return market.Quote{Instrument: mnq, Time: at, Bid: bid, Ask: bid + 1, BidSize: 0, AskSize: 50}
}

// Scenario: a stop cancelled for want of liquidity is proved, not believed
//
// It leaves no position change, so for a while this was the one protective
// transition a reader took on trust. It never had to be. Replay holds the
// observation, the leg's side, price and quantity, and what earlier fills took
// out of the book, and ConservativeExecution is a pure function of those —
// StopTriggered exists precisely to separate a level never reached from a level
// reached with nothing behind it.
func TestAStopCancelledForWantOfLiquidityIsProved(t *testing.T) {
	honest := func(t *testing.T) *session.Session {
		t.Helper()
		s := protectedLong(t)
		mustObserve(t, s, emptyBid(4_000, 19_800))
		return s
	}

	t.Run("the market agrees, and it is accepted", func(t *testing.T) {
		s := honest(t)
		if got, want := cancellations(s.Events()), 1; len(got) != want {
			t.Fatalf("cancellations: got %v, want one", got)
		}
		checked(t, s)
	})

	// Each forgery leaves the log internally coherent and contradicts only the
	// market, which is exactly the class this check exists for.
	tests := []struct {
		name  string
		forge func(*testing.T, []session.Event)
	}{
		{"the level was never reached", func(t *testing.T, e []session.Event) {
			at := indexOfKind(t, e, session.KindProtectionPlaced, 1)
			v := e[at].(session.ProtectionPlaced)
			v.StopPrice = 19_700
			e[at] = v
		}},
		{"the book still showed size for it", func(t *testing.T, e []session.Event) {
			// Sizes feed no valuation, so nothing but the re-execution notices.
			at := indexOfKind(t, e, session.KindMarketObserved, 2)
			v := e[at].(session.MarketObserved)
			v.Quote.BidSize = 50
			e[at] = v
		}},
		{"a different quantity was withdrawn", func(t *testing.T, e []session.Event) {
			at := indexOfKind(t, e, session.KindOrderCancelled, 1)
			v := e[at].(session.OrderCancelled)
			v.RemainingQty = 4
			e[at] = v
		}},
		{"the sibling is blamed for it", func(t *testing.T, e []session.Event) {
			at := indexOfKind(t, e, session.KindOrderCancelled, 1)
			v := e[at].(session.OrderCancelled)
			v.Reason = session.CancelledByOCO
			e[at] = v
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events := honest(t).Events()
			tc.forge(t, events)
			if _, err := session.Replay(events); !errors.Is(err, session.ErrFabricated) {
				t.Fatalf("Replay: got %v, want %v", err, session.ErrFabricated)
			}
		})
	}
}

// Scenario: a stop finds nothing because an earlier order took it
//
//	Given a working order that consumes the bid on this observation
//	And an entry behind it whose fill activates a stop at the same level
//	Then the stop is cancelled whole, and Replay accepts it — because Replay
//	  consumes the book in the same order and finds it empty too.
//
// This is the case the check would get wrong if it re-executed against the
// quote as it arrived rather than as it stood. It is also the case a journal
// could lie about by reordering: claiming the protection was offered the book
// first, when what it actually met was what the order in front had left.
func TestAStopFindsWhatAnEarlierOrderLeft(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	mustSubmit(t, s, stopOrder(t, "ahead", market.SideSell, 2, 19_600))
	mustProtect(t, s, limitOrder(t, "entry", market.SideBuy, 8, 19_500), 19_450, 20_400)

	// Two contracts on the bid and eight on the ask: the order in front takes
	// the whole bid, then the entry fills and flips the position long, and the
	// stop it activates reaches its level with nothing left.
	mustObserve(t, s, market.Quote{
		Instrument: mnq, Time: 4_000, Bid: 19_400, Ask: 19_401, BidSize: 2, AskSize: 8,
	})

	position, _ := s.Account().Position(mnq)
	if position.NetQty != 6 {
		t.Fatalf("position: got %d, want the 6 the flip left open", position.NetQty)
	}
	active := onlyActive(t, s)
	if active.StopPrice != 0 || active.ProtectedQty != 6 {
		t.Fatalf("protection: got %+v, want the stop gone over 6 contracts", active)
	}
	// The order in front filled whole, so it left no remainder to cancel. The
	// only cancellation is the leg that arrived to an empty bid.
	stopLeg, _ := legsOf(t, s)
	if got, want := cancellations(s.Events()), []cancellation{
		{stopLeg, 6, "unfillable remainder"},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cancellations\n got: %v\nwant: %v", got, want)
	}
	checked(t, s)

	// And a resumed session carries on from the same book the interrupted one
	// left, which is the whole reason the re-execution can be trusted.
	state, err := session.Replay(s.Events())
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	resumed, err := session.Resume(state, nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	mustSubmit(t, resumed, order("after", market.SideSell, 3))
	continued := s
	mustSubmit(t, continued, order("after", market.SideSell, 3))
	if !reflect.DeepEqual(resumed.Events(), continued.Events()) {
		t.Fatal("a resumed session traded a different book from the one it inherited")
	}
}

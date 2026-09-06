package session_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"praxis/internal/challenge"
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

// Scenario: a remainder cancelled as unfillable is proved against the book
//
//	Given an order that took everything the book showed and had the rest
//	  cancelled
//	When the observation is forged to have shown more
//	Then Replay refuses it.
//
// The book at the moment a remainder is cancelled is the right book to judge it
// against, and for a while this was believed to be the wrong one. The fills are
// recorded before the cancellation and Replay consumes them, so what is left is
// exactly what the remainder met: if anything is still there, the remainder was
// not unfillable and the order should have taken it.
func TestARemainderCancelledAsUnfillableIsProved(t *testing.T) {
	honest := func(t *testing.T) *session.Session {
		t.Helper()
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		mustObserve(t, s, sized(3_000, 20_000, 20_001, 3))
		mustSubmit(t, s, order("o1", market.SideBuy, 5))
		return s
	}

	t.Run("the market agrees, and it is accepted", func(t *testing.T) {
		s := honest(t)
		if got, want := cancellations(s.Events()), []cancellation{
			{"o1", 2, "unfillable remainder"},
		}; !reflect.DeepEqual(got, want) {
			t.Fatalf("cancellations\n got: %v\nwant: %v", got, want)
		}
		checked(t, s)
	})

	tests := []struct {
		name  string
		forge func(*testing.T, []session.Event)
	}{
		{"the book had shown more than it gave", func(t *testing.T, e []session.Event) {
			at := indexOfKind(t, e, session.KindMarketObserved, 1)
			v := e[at].(session.MarketObserved)
			v.Quote.AskSize = 10
			e[at] = v
		}},
		{"a different quantity was withdrawn", func(t *testing.T, e []session.Event) {
			at := indexOfKind(t, e, session.KindOrderCancelled, 1)
			v := e[at].(session.OrderCancelled)
			v.RemainingQty = 1
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

// Scenario: a discretionary withdrawal cannot be relabelled as a mechanical one
//
// Only two things leave an unfillable remainder: a market order, which does not
// rest because resting it would invent a price the trader never named, and a
// stop that has triggered and therefore cannot go back to waiting. A limit
// rests, always.
//
// The reason is not decoration. Calling "the trader withdrew their stop" a
// remainder the book could not fill turns a discretionary decision into a
// mechanical event, and that decision is the numerator of a hypothesis this log
// exists to measure. A journal that could spell one as the other would be
// measuring the wrong thing while reading perfectly.
func TestACancellationCannotBorrowTheWrongReason(t *testing.T) {
	tests := []struct {
		name  string
		build func(*testing.T) *session.Session
	}{
		{"a stop the market never reached", func(t *testing.T) *session.Session {
			s := newSession(t)
			mustOpen(t, s, 2_000, "d1")
			mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
			mustSubmit(t, s, order("long", market.SideBuy, 2))
			mustSubmit(t, s, stopOrder(t, "protect", market.SideSell, 2, 19_000))
			if err := s.CancelOrder("protect"); err != nil {
				t.Fatalf("CancelOrder: %v", err)
			}
			return s
		}},
		{"a limit the trader took back", func(t *testing.T) *session.Session {
			s := newSession(t)
			mustOpen(t, s, 2_000, "d1")
			mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
			mustSubmit(t, s, limitOrder(t, "bid", market.SideBuy, 2, 19_000))
			if err := s.CancelOrder("bid"); err != nil {
				t.Fatalf("CancelOrder: %v", err)
			}
			return s
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.build(t)
			events := s.Events()
			at := indexOfKind(t, events, session.KindOrderCancelled, 1)
			if got := events[at].(session.OrderCancelled).Reason; got != session.CancelledByTrader {
				t.Fatalf("the fixture cancelled for %v, not by the trader", got)
			}
			checked(t, s)

			// The forgery: the same cancellation, blamed on the book.
			forged := events[at].(session.OrderCancelled)
			forged.Reason = session.CancelledUnfillableRemainder
			events[at] = forged

			if _, err := session.Replay(events); !errors.Is(err, session.ErrFabricated) {
				t.Fatalf("Replay: got %v, want %v", err, session.ErrFabricated)
			}
		})
	}
}

// Scenario: an ending nothing owed still has to hold what it claims to hold
//
// A protection the trader withdraws produces an ending nothing derived, so the
// owed queue never sees it and only Verify's own check stands between the log
// and a forged pair of levels. Those levels are the whole record of what cover
// existed at the moment it was given up.
func TestAWithdrawalCannotMisstateWhatItGaveUp(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	mustProtect(t, s, limitOrder(t, "entry", market.SideBuy, 2, 19_000), 18_900, 19_500)
	if err := s.CancelProtection(entryRef("entry")); err != nil {
		t.Fatalf("CancelProtection: %v", err)
	}
	events := s.Events()
	if err := session.Verify(events); err != nil {
		t.Fatalf("the fixture does not verify: %v", err)
	}

	at := indexOfKind(t, events, session.KindProtectionEnded, 1)
	ended := events[at].(session.ProtectionEnded)
	if ended.Reason != session.ProtectionWithdrawnByTrader {
		t.Fatalf("the fixture ended for %v, not by withdrawal", ended.Reason)
	}
	ended.StopPrice = 19_400
	events[at] = ended

	if err := session.Verify(events); !errors.Is(err, session.ErrContradictoryLog) {
		t.Fatalf("Verify: got %v, want %v", err, session.ErrContradictoryLog)
	}
}

// Scenario: something the market reached cannot simply go on waiting
//
// This is the forgery that was accepted until now, and it is the only class
// that flatters a trader: everything that happened was proved against the
// aggregates, and nothing that did not happen was proved against anything.
//
// The account is deliberately flat in the order cases, so that forging the
// book changes no valuation and the survivor check is the only thing standing
// between the log and the lie. A naked stop is unusual but legal, and it is the
// cleanest way to say that.
func TestSomethingTheMarketReachedCannotGoOnWaiting(t *testing.T) {
	// waiting builds a journal with one resting order that the market did not
	// reach, optionally followed by a further observation — so that both the
	// per-observation check and the one at the end of the log are exercised.
	waiting := func(t *testing.T, resting market.Order, andThen bool) *session.Session {
		t.Helper()
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
		mustSubmit(t, s, resting)
		mustObserve(t, s, sized(4_000, 20_010, 20_011, 50))
		if andThen {
			mustObserve(t, s, sized(5_000, 20_020, 20_021, 50))
		}
		return s
	}

	forgeQuote := func(bid market.Ticks, size market.Qty) func(*testing.T, []session.Event) {
		return func(t *testing.T, e []session.Event) {
			at := indexOfKind(t, e, session.KindMarketObserved, 2)
			v := e[at].(session.MarketObserved)
			v.Quote.Bid, v.Quote.Ask = bid, bid+1
			v.Quote.BidSize = size
			e[at] = v
		}
	}

	tests := []struct {
		name    string
		resting func(*testing.T) market.Order
		forge   func(*testing.T, []session.Event)
		says    string
	}{
		{
			// Triggering owes nothing to liquidity, so an empty book is no
			// excuse: the stop became a market order and cannot untrigger.
			name:    "a stop the market went through, with nothing behind it",
			resting: func(t *testing.T) market.Order { return stopOrder(t, "protect", market.SideSell, 5, 19_900) },
			forge:   forgeQuote(19_800, 0),
			says:    "reached its stop at",
		},
		{
			name:    "a stop the market went through, with depth behind it",
			resting: func(t *testing.T) market.Order { return stopOrder(t, "protect", market.SideSell, 5, 19_900) },
			forge:   forgeQuote(19_800, 50),
			says:    "reached its stop at",
		},
		{
			// A limit is never triggered, so only the fills say it should have
			// gone.
			name:    "a limit the market came to",
			resting: func(t *testing.T) market.Order { return limitOrder(t, "offer", market.SideSell, 5, 20_100) },
			forge:   forgeQuote(20_150, 50),
			says:    "still showed",
		},
	}

	for _, tc := range tests {
		for _, andThen := range []bool{false, true} {
			where := "at the end of the log"
			if andThen {
				where = "with another observation after it"
			}
			t.Run(tc.name+", "+where, func(t *testing.T) {
				honest := waiting(t, tc.resting(t), andThen)
				if len(honest.WorkingOrders()) != 1 {
					t.Fatalf("the fixture filled: %+v", honest.WorkingOrders())
				}
				checked(t, honest)

				events := waiting(t, tc.resting(t), andThen).Events()
				tc.forge(t, events)
				_, err := session.Replay(events)
				if !errors.Is(err, session.ErrFabricated) {
					t.Fatalf("Replay: got %v, want %v", err, session.ErrFabricated)
				}
				// And for the right reason, named precisely. A forged book
				// that also moved a valuation would be caught by the wrong
				// check entirely, and the two halves of this one — the level
				// was reached, the depth was still there — subsume each other
				// unless the test says which it expects.
				if !strings.Contains(err.Error(), "is still working") || !strings.Contains(err.Error(), tc.says) {
					t.Fatalf("rejected for another reason: %v", err)
				}
			})
		}
	}
}

// The same for a protective leg, where the account cannot be flat — so the
// forgery moves the level rather than the market, which changes no valuation.
//
// The two halves need different legs to separate them. A stop that the depth
// would have filled has also been triggered, and triggering is checked first,
// so only a target — which is a limit and is never triggered — can be reached
// by the depth half alone.
func TestAProtectiveLevelTheMarketReachedCannotGoOnWaiting(t *testing.T) {
	honest := func(t *testing.T, bidDepth market.Qty) *session.Session {
		t.Helper()
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		mustObserve(t, s, market.Quote{
			Instrument: mnq, Time: 3_000, Bid: 20_000, Ask: 20_001,
			BidSize: bidDepth, AskSize: 50,
		})
		mustProtect(t, s, order("entry", market.SideBuy, 5), 19_900, 20_400)
		mustObserve(t, s, sized(4_000, 20_010, 20_011, 50))
		return s
	}

	tests := []struct {
		name     string
		bidDepth market.Qty
		forge    func(*session.ProtectionPlaced)
		says     string
	}{
		{
			// A stop above the market with nothing on the bid: it triggered,
			// and triggering owes nothing to liquidity.
			name: "a stop the market reached, with nothing behind it", bidDepth: 0,
			forge: func(p *session.ProtectionPlaced) { p.StopPrice = 20_050 },
			says:  "reached its level at",
		},
		{
			// A target below the market with depth on the bid: nothing
			// triggered, and only the depth says it should have gone.
			name: "a target the market came to", bidDepth: 50,
			forge: func(p *session.ProtectionPlaced) { p.TargetPrice = 19_950 },
			says:  "still showed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := honest(t, tc.bidDepth)
			if len(s.ActiveProtections()) != 1 {
				t.Fatalf("the fixture has no protection: %+v", s.ActiveProtections())
			}
			checked(t, s)

			events := honest(t, tc.bidDepth).Events()
			at := indexOfKind(t, events, session.KindProtectionPlaced, 1)
			placed := events[at].(session.ProtectionPlaced)
			tc.forge(&placed)
			events[at] = placed

			_, err := session.Replay(events)
			if !errors.Is(err, session.ErrFabricated) {
				t.Fatalf("Replay: got %v, want %v", err, session.ErrFabricated)
			}
			if !strings.Contains(err.Error(), "still protects episode") || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("rejected for another reason: %v", err)
			}
		})
	}
}

// Scenario: nothing is asked of an order no session was open to offer it to
//
// The live session offers an observation to what is waiting only while a
// trading session is open and the evaluation has not ended. A reader that
// demanded an explanation from an order nobody offered anything to would
// reject a journal the engine itself produces.
func TestAnOrderNobodyWasOfferedIsAskedNothing(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	mustSubmit(t, s, stopOrder(t, "protect", market.SideSell, 5, 19_900))
	if err := s.EndTradingSession(4_000); err != nil {
		t.Fatalf("EndTradingSession: %v", err)
	}

	// The market walks straight through the stop with no session open, so
	// nothing is offered it and nothing happens to it.
	mustObserve(t, s, sized(5_000, 19_800, 19_801, 50))
	mustObserve(t, s, sized(6_000, 19_700, 19_701, 50))

	if len(s.WorkingOrders()) != 1 {
		t.Fatalf("working: got %+v, want the stop untouched", s.WorkingOrders())
	}
	checked(t, s)
}

// Scenario: a limit that could not fill because someone was ahead of it is
//
//	accepted
//
// The check judges against the book as it was left, not as it arrived, which
// is why it can be this strict without a second copy of the resolution order.
// Everything that filled has already been consumed; what remains is exactly
// what the survivors were offered. A limit whose price the market reached but
// whose depth an order in front of it took has nothing to answer for — and a
// stop at the same level does, because triggering owes nothing to liquidity.
func TestAnOrderStarvedByTheOneAheadOfItIsAccepted(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	// Two sell limits at the same price, submitted in order. The first one to
	// be offered the book takes all three contracts it shows.
	mustSubmit(t, s, order("long", market.SideBuy, 8))
	mustSubmit(t, s, limitOrder(t, "ahead", market.SideSell, 3, 20_100))
	mustSubmit(t, s, limitOrder(t, "behind", market.SideSell, 3, 20_100))
	mustObserve(t, s, sized(4_000, 20_150, 20_151, 3))

	working := s.WorkingOrders()
	if len(working) != 1 || working[0].ID != "behind" {
		t.Fatalf("working: got %+v, want only the order behind still waiting", working)
	}
	if position, _ := s.Account().Position(mnq); position.NetQty != 5 {
		t.Fatalf("position: got %d, want only the order in front to have filled", position.NetQty)
	}
	// It survived an observation whose price it had reached, and the log is
	// accepted, because the book it was actually offered was empty.
	checked(t, s)
}

// The evaluation ending stops the offering too, and a reader that forgot it
// would reject a journal the engine produces.
func TestAnOrderIsAskedNothingOnceTheEvaluationHasEnded(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	// A buy stop far above the market, and a position large enough that the
	// next observation ends the evaluation.
	mustSubmit(t, s, stopOrder(t, "later", market.SideBuy, 1, 21_000))
	mustSubmit(t, s, order("long", market.SideBuy, 10))

	mustObserve(t, s, sized(4_000, 19_800, 19_801, 50))
	if s.Challenge().State() != challenge.StateFailed {
		t.Fatalf("the fixture did not end the evaluation: %v", s.Challenge().State())
	}

	// The market now walks through the resting stop. Nothing is offered it,
	// because the evaluation is over.
	mustObserve(t, s, sized(5_000, 21_100, 21_101, 50))
	mustObserve(t, s, sized(6_000, 21_200, 21_201, 50))

	if len(s.WorkingOrders()) != 1 {
		t.Fatalf("working: got %+v, want the stop untouched", s.WorkingOrders())
	}
	checked(t, s)
}

// Scenario: a fill is re-executed against the book it met
//
// A fill's price is the plainest place the rule "execution lies in favour of
// the market, never the trader" can be broken and leave no trace: a market buy
// recorded ten ticks below the ask is ten ticks of free improvement, and every
// later check agrees with it, because the fill is what fed the account and a
// valuation compares the account with itself.
//
// Quantity is recomputable for the same reason the book is consumed at all: a
// fill is exactly what the policy produced against the book at that moment, and
// every earlier fill has already been taken out of it.
func TestAFillIsReExecutedAgainstTheBookItMet(t *testing.T) {
	honest := func(t *testing.T) *session.Session {
		t.Helper()
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		mustObserve(t, s, sized(3_000, 20_000, 20_010, 3))
		mustSubmit(t, s, order("o1", market.SideBuy, 5))
		return s
	}

	t.Run("the book agrees, and it is accepted", func(t *testing.T) {
		s := honest(t)
		var got market.Fill
		for _, e := range s.Events() {
			if v, ok := e.(session.FillProduced); ok {
				got = v.Fill
			}
		}
		// It crossed the spread and took only the depth the quote showed.
		if got.Price != 20_010 || got.Qty != 3 {
			t.Fatalf("fill: got %d at %d, want 3 at the ask of 20010", got.Qty, got.Price)
		}
		checked(t, s)
	})

	tests := []struct {
		name  string
		forge func(*market.Fill)
		says  string
	}{
		{"a better price than the market offered", func(f *market.Fill) { f.Price = 20_000 }, "would have given it 20010"},
		{"more depth than the book showed", func(f *market.Fill) { f.Qty = 5 }, "would have given it 3"},
		{"a fill nobody's order produced", func(f *market.Fill) { f.OrderID = "ghost" }, "nothing submitted it"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events := honest(t).Events()
			at := indexOfKind(t, events, session.KindFillProduced, 1)
			produced := events[at].(session.FillProduced)
			tc.forge(&produced.Fill)
			events[at] = produced

			_, err := session.Replay(events)
			if !errors.Is(err, session.ErrFabricated) {
				t.Fatalf("Replay: got %v, want %v", err, session.ErrFabricated)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("rejected for another reason: %v", err)
			}
		})
	}
}

// Scenario: a book cannot be overdrawn
//
// Execution stops at the size an observation displayed, so a live session
// cannot produce this. A journal can, and a book driven negative is the
// arithmetic saying a fill took depth that was never there. Nothing looks at
// the leftovers unless something is still waiting on them, which is why this
// has to be the subtraction's own business.
func TestAFillCannotTakeMoreThanTheBookShowed(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_010, 3))
	mustSubmit(t, s, order("o1", market.SideBuy, 3))
	events := s.Events()

	// The observation is forged to have shown less than it gave. The fill's own
	// re-execution catches it first; the subtraction is what would have caught
	// it if nothing else did.
	for _, tc := range []struct {
		name string
		size market.Qty
		says string
	}{
		{"a book that showed less", 1, "would have given it 1"},
		{"a book that showed nothing at all", 0, "would have given it nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forged := make([]session.Event, len(events))
			copy(forged, events)
			at := indexOfKind(t, forged, session.KindMarketObserved, 1)
			observed := forged[at].(session.MarketObserved)
			observed.Quote.AskSize = tc.size
			forged[at] = observed

			_, err := session.Replay(forged)
			if !errors.Is(err, session.ErrFabricated) {
				t.Fatalf("Replay: got %v, want %v", err, session.ErrFabricated)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("rejected for another reason: %v", err)
			}
		})
	}

	// And the subtraction refuses it on its own terms.
	overdrawn := []market.Fill{
		{OrderID: "o1", Instrument: mnq, Time: 3_000, Side: market.SideBuy, Price: 20_010, Qty: 3},
	}
	if _, err := session.ConsumeBookForTest(sized(3_000, 20_000, 20_010, 1), overdrawn); !errors.Is(err, session.ErrOverdrawnBook) {
		t.Fatalf("consuming: got %v, want %v", err, session.ErrOverdrawnBook)
	}
}

// Scenario: an order cannot simply stop being mentioned
//
// Every order reaches one of three ends: it fills, it is cancelled, or it is
// still waiting when the log stops. One that reaches none is invisible to every
// other check — it never enters the working set, so nothing asks it to account
// for surviving an observation, and it moved no money, so no valuation
// disagrees. A marketable limit the log quietly forgets is a decision the
// trader made and the record does not contain.
func TestAnOrderCannotSimplyStopBeingMentioned(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	mustSubmit(t, s, limitOrder(t, "o1", market.SideBuy, 5, 20_100))

	// Everything the fill caused is cut out, and the valuation is adjusted to
	// the flat account that leaves — so the log is coherent with its own lie.
	var kept []session.Event
	for _, e := range s.Events() {
		switch e.(type) {
		case session.FillProduced, session.PositionChanged:
			continue
		}
		kept = append(kept, e)
	}
	events := renumber(kept)
	for n, e := range events {
		if v, ok := e.(session.AccountValued); ok {
			v.BalanceCts, v.EquityCts = 5_000_000, 5_000_000
			events[n] = v
		}
	}

	// Verify sees nothing wrong: every context still adds up.
	if err := session.Verify(events); err != nil {
		t.Fatalf("the forgery is not internally coherent, so it proves less than it should: %v", err)
	}
	_, err := session.Replay(events)
	if !errors.Is(err, session.ErrFabricated) {
		t.Fatalf("Replay: got %v, want %v", err, session.ErrFabricated)
	}
	if !strings.Contains(err.Error(), "neither rests nor cancels it") {
		t.Fatalf("rejected for another reason: %v", err)
	}
}

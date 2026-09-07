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
	// One act, issued to both: the same person doing the same thing once, which
	// is what makes the two journals comparable at all.
	after, act := order("after", market.SideSell, 3), decided(0)
	if err := resumed.SubmitOrder(after, act); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
	if err := s.SubmitOrder(after, act); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
	if !reflect.DeepEqual(resumed.Events(), s.Events()) {
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
			if err := s.CancelOrder("protect", decidedAt()); err != nil {
				t.Fatalf("CancelOrder: %v", err)
			}
			return s
		}},
		{"a limit the trader took back", func(t *testing.T) *session.Session {
			s := newSession(t)
			mustOpen(t, s, 2_000, "d1")
			mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
			mustSubmit(t, s, limitOrder(t, "bid", market.SideBuy, 2, 19_000))
			if err := s.CancelOrder("bid", decidedAt()); err != nil {
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
			// A remainder the book could not fill is nobody's decision, so a
			// convincing forgery drops the clock with the reason.
			forged.Reason, forged.Decided = session.CancelledUnfillableRemainder, session.Decision{}
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
	if err := s.CancelProtection(entryRef("entry"), decidedAt()); err != nil {
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

// Scenario: a fill's side, time and instrument are checked too
//
// They are the least interesting third of the fill's re-execution and the
// easiest to leave unexercised, which is exactly what had happened. Time is
// not decoration: it is the only stamp a fill carries, and a hypothesis about
// how long a trader waits is measured on stamps.
func TestAFillsSideTimeAndInstrumentAreChecked(t *testing.T) {
	honest := func(t *testing.T) []session.Event {
		t.Helper()
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		mustObserve(t, s, sized(3_000, 20_000, 20_010, 5))
		mustSubmit(t, s, order("o1", market.SideBuy, 3))
		return s.Events()
	}

	tests := []struct {
		name  string
		forge func(*market.Fill)
	}{
		{"a fill on the other side", func(f *market.Fill) { f.Side = market.SideSell }},
		{"a fill stamped at another moment", func(f *market.Fill) { f.Time = 3_001 }},
		{"a fill in another instrument", func(f *market.Fill) {
			f.Instrument = market.Instrument{Symbol: "MES", CentsPerTick: 125}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events := honest(t)
			at := indexOfKind(t, events, session.KindFillProduced, 1)
			produced := events[at].(session.FillProduced)
			tc.forge(&produced.Fill)
			events[at] = produced

			_, err := session.Replay(events)
			if !errors.Is(err, session.ErrFabricated) {
				t.Fatalf("Replay: got %v, want %v", err, session.ErrFabricated)
			}
			// Named precisely: an instrument nobody traded is also caught by
			// the account a moment later, and the test would not notice which.
			if !strings.Contains(err.Error(), "the order and the book produce") {
				t.Fatalf("rejected for another reason: %v", err)
			}
		})
	}
}

// Scenario: the journal records when a person acted, and it is not market time
//
// Two orders sent between one tick and the next carry the same envelope time,
// because the market did not move. The interval between them is the thing a
// hypothesis about hesitation measures, and it exists only if somebody wrote it
// down. The kernel writes it and never reads it: nothing here decides anything
// on a human clock, which is why rule 2 is untouched.
func TestTheJournalRecordsWhenAPersonActed(t *testing.T) {
	first, second := decided(0), decided(40_000_000_000) // forty seconds later

	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	if err := s.SubmitOrder(order("o1", market.SideBuy, 1), first); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
	if err := s.SubmitOrder(order("o2", market.SideBuy, 1), second); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}

	var submitted []session.OrderSubmitted
	for _, e := range s.Events() {
		if v, ok := e.(session.OrderSubmitted); ok {
			submitted = append(submitted, v)
		}
	}
	if len(submitted) != 2 {
		t.Fatalf("orders: got %d, want 2", len(submitted))
	}
	// The market did not move between them, and the log says so.
	if submitted[0].Time != submitted[1].Time {
		t.Fatalf("market time moved: %d then %d", submitted[0].Time, submitted[1].Time)
	}
	// The person took forty seconds, and the log says that too — measured on
	// the monotonic reading, which is what an interval is computed from.
	if submitted[0].Decided.Segment != submitted[1].Decided.Segment {
		t.Fatal("the two decisions are in different segments and cannot be subtracted")
	}
	if got := submitted[1].Decided.ElapsedNanos - submitted[0].Decided.ElapsedNanos; got != 40_000_000_000 {
		t.Fatalf("the interval between decisions is %dns, want 40s", got)
	}
	checked(t, s)

	// A cancellation the trader asked for carries their clock; one the system
	// derived carries nothing, because nobody decided it.
	if err := s.CancelOrder("o1", decided(50_000_000_000)); !errors.Is(err, session.ErrNoSuchOrder) {
		t.Fatalf("the fixture left a working order: %v", err)
	}
	for _, e := range s.Events() {
		if v, ok := e.(session.OrderCancelled); ok && v.Reason != session.CancelledByTrader && !v.Decided.IsZero() {
			t.Fatalf("a cancellation nobody decided claims a human clock: %+v", v)
		}
	}
}

// Scenario: what the system derived cannot claim a person decided it
//
// A protection ended because a fill closed the position, and a leg cancelled
// because its sibling executed, are events the log itself required. Crediting
// either to a person's clock would put a decision in the record that nobody
// made — and the record exists to hold decisions.
func TestWhatWasDerivedCannotClaimAPersonDecidedIt(t *testing.T) {
	tests := []struct {
		name  string
		kind  session.Kind
		forge func(session.Event) session.Event
	}{
		{"an ending a fill required", session.KindProtectionEnded, func(e session.Event) session.Event {
			v := e.(session.ProtectionEnded)
			v.Decided = decidedAt()
			return v
		}},
		{"a leg its sibling cancelled", session.KindOrderCancelled, func(e session.Event) session.Event {
			v := e.(session.OrderCancelled)
			v.Decided = decidedAt()
			return v
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := protectedLong(t)
			mustObserve(t, s, sized(4_000, 20_500, 20_501, 50))
			events := s.Events()
			at := indexOfKind(t, events, tc.kind, 1)
			events[at] = tc.forge(events[at])

			// Structure, not arithmetic — the same family as a trading session
			// opened while another is open, and one rule that both readers
			// ask, because a second copy of it would eventually disagree.
			if _, err := session.Replay(events); !errors.Is(err, session.ErrStructure) {
				t.Fatalf("Replay: got %v, want %v", err, session.ErrStructure)
			}
			if err := session.Verify(events); !errors.Is(err, session.ErrContradictoryLog) {
				t.Fatalf("Verify: got %v, want %v", err, session.ErrContradictoryLog)
			}
		})
	}
}

// Scenario: the monotonic reading cannot go back, and the wall clock may
//
// A wall clock is for audit — saying when in the world something happened — and
// it moves backwards legitimately: a time server corrects it, an operator sets
// it, a suspended machine resumes. Refusing a corrected clock would refuse a
// session that was entirely honest, and one connection to one kernel does not
// make a wall clock monotonic. So intervals are computed from the monotonic
// reading instead, and that one is held to its discipline.
func TestTheMonotonicReadingCannotGoBack(t *testing.T) {
	submit := func(t *testing.T, s *session.Session, id string, d session.Decision) error {
		t.Helper()
		return s.SubmitOrder(order(id, market.SideBuy, 1), d)
	}
	open := func(t *testing.T) *session.Session {
		t.Helper()
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
		return s
	}

	t.Run("a wall clock corrected backwards is accepted", func(t *testing.T) {
		s := open(t)
		// Five seconds of monotonic time passed; the world's clock was set
		// back four seconds in between, which is what a time server does.
		if err := submit(t, s, "o1", session.Decision{GestureID: "g-a", AtUTCNanos: 5_000_000_000, Segment: 1, ElapsedNanos: 0}); err != nil {
			t.Fatalf("SubmitOrder: %v", err)
		}
		if err := submit(t, s, "o2", session.Decision{GestureID: "g-b", AtUTCNanos: 1_000_000_000, Segment: 1, ElapsedNanos: 5_000_000_000}); err != nil {
			t.Fatalf("SubmitOrder: %v", err)
		}
		checked(t, s)
	})

	t.Run("a new segment may begin anywhere", func(t *testing.T) {
		// A recovery ends a segment: nothing carries across it, so the next
		// one starts from its own zero and no interval spans the two.
		s := open(t)
		if err := submit(t, s, "o1", session.Decision{GestureID: "g-a", AtUTCNanos: 1_000, Segment: 1, ElapsedNanos: 90_000_000_000}); err != nil {
			t.Fatalf("SubmitOrder: %v", err)
		}
		if err := submit(t, s, "o2", session.Decision{GestureID: "g-b", AtUTCNanos: 2_000, Segment: 2, ElapsedNanos: 0}); err != nil {
			t.Fatalf("SubmitOrder: %v", err)
		}
		checked(t, s)
	})

	refused := []struct {
		name         string
		first, later session.Decision
	}{
		{"the monotonic reading goes back inside one segment",
			session.Decision{GestureID: "g-a", AtUTCNanos: 1_000, Segment: 1, ElapsedNanos: 5_000_000_000},
			session.Decision{GestureID: "g-b", AtUTCNanos: 2_000, Segment: 1, ElapsedNanos: 1_000_000_000}},
		{"a segment is returned to",
			session.Decision{GestureID: "g-a", AtUTCNanos: 1_000, Segment: 2, ElapsedNanos: 0},
			session.Decision{GestureID: "g-b", AtUTCNanos: 2_000, Segment: 1, ElapsedNanos: 0}},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			s := open(t)
			if err := submit(t, s, "o1", tc.first); err != nil {
				t.Fatalf("SubmitOrder: %v", err)
			}
			// The kernel records what it is given; nothing here decides on a
			// human clock, so this is accepted at the door and refused by the
			// readers.
			if err := submit(t, s, "o2", tc.later); err != nil {
				t.Fatalf("SubmitOrder: %v", err)
			}
			if _, err := session.Replay(s.Events()); !errors.Is(err, session.ErrStructure) {
				t.Fatalf("Replay: got %v, want %v", err, session.ErrStructure)
			}
			if err := session.Verify(s.Events()); !errors.Is(err, session.ErrContradictoryLog) {
				t.Fatalf("Verify: got %v, want %v", err, session.ErrContradictoryLog)
			}
		})
	}
}

// Scenario: a journal cannot say both that somebody traded it and that nobody
//
//	decided anything in it
//
// Zero means no person was there. Once that is established the converse is a
// rule worth holding, because the way it breaks is an interface that forgets to
// stamp the clock — or stamps it on three of the four commands — and nothing
// notices until the analysis, by which time the timing data for that pilot
// session no longer exists.
func TestASubjectAndAClockMustAgree(t *testing.T) {
	t.Run("a subject who decided nothing", func(t *testing.T) {
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
		if err := s.SubmitOrder(order("o1", market.SideBuy, 1), session.Decision{}); err != nil {
			t.Fatalf("SubmitOrder: %v", err)
		}
		if _, err := session.Replay(s.Events()); !errors.Is(err, session.ErrStructure) {
			t.Fatalf("Replay: got %v, want %v", err, session.ErrStructure)
		}
	})

	t.Run("decisions nobody is recorded as having made", func(t *testing.T) {
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
		mustSubmit(t, s, order("o1", market.SideBuy, 1))

		events := s.Events()
		started := events[0].(session.SessionStarted)
		started.Config.SubjectID = ""
		events[0] = started

		if _, err := session.Replay(events); !errors.Is(err, session.ErrStructure) {
			t.Fatalf("Replay: got %v, want %v", err, session.ErrStructure)
		}
	})

	t.Run("a journal nobody traded, decided by nobody", func(t *testing.T) {
		// A scripted run: nobody, nothing presented to them, and no decision
		// anywhere — the honest shape of a journal a person never touched.
		cfg := config()
		cfg.SubjectID, cfg.Pacing = "", session.PacingScripted
		s, err := session.New(cfg, 1_000, nil)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		mustOpen(t, s, 2_000, "d1")
		mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
		if err := s.SubmitOrder(order("o1", market.SideBuy, 1), session.Decision{}); err != nil {
			t.Fatalf("SubmitOrder: %v", err)
		}
		checked(t, s)
	})
}

// Scenario: a name the record cannot hold is refused at the door
//
//	Given an order, a trading session or a subject named with a character the
//	  journal has no way to write
//	Then the command is refused, and nothing is recorded.
//
// Before this, all three were accepted by the domain and refused by the codec —
// at commit time, three good batches in, taking the session with them and
// refusing every perfectly valid order after it. A system whose entire purpose
// is the record cannot let a decision exist that the record has no way to
// contain, and this gets much worse the moment an interface mints identifiers
// from whatever a person typed.
func TestANameTheRecordCannotHoldIsRefused(t *testing.T) {
	bad := []string{"o 1", "o\n1", "órden-1", "o/1", "o+1", "o=1", "orden#1", "o\t1"}

	t.Run("an order", func(t *testing.T) {
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
		before := s.JournalLen()

		for _, id := range bad {
			o := market.Order{
				ID: id, Instrument: mnq, Side: market.SideBuy,
				Type: market.OrderTypeMarket, Qty: 1,
			}
			if err := s.SubmitOrder(o, decidedAt()); !errors.Is(err, market.ErrIdentifierCharacter) {
				t.Fatalf("%q: got %v, want %v", id, err, market.ErrIdentifierCharacter)
			}
		}
		if s.JournalLen() != before || s.NeedsRecovery() != nil {
			t.Fatal("a refused name recorded something, or stopped the session")
		}
		// And the session is still usable, which is the whole point.
		mustSubmit(t, s, order("o-1", market.SideBuy, 1))
		checked(t, s)
	})

	t.Run("a trading session", func(t *testing.T) {
		s := newSession(t)
		if err := s.OpenTradingSession(2_000, "2026/08/27"); !errors.Is(err, market.ErrIdentifierCharacter) {
			t.Fatalf("got %v, want %v", err, market.ErrIdentifierCharacter)
		}
		// The evaluation did not move either: a boundary the journal refuses
		// must not have been accepted by the challenge first.
		mustOpen(t, s, 2_000, "2026-08-27")
		checked(t, s)
	})

	t.Run("a subject", func(t *testing.T) {
		cfg := config()
		cfg.SubjectID = "P-01 pilot"
		if _, err := session.New(cfg, 1_000, nil); !errors.Is(err, market.ErrIdentifierCharacter) {
			t.Fatalf("got %v, want %v", err, market.ErrIdentifierCharacter)
		}
	})

	t.Run("an instrument", func(t *testing.T) {
		// The symbol is written into the first event of every journal, so one
		// the record cannot hold is not a bad instrument: it is a journal that
		// cannot begin. It comes from a market file's header, which is outside.
		for _, symbol := range []string{"MN Q", "MNQ/1", "MNQ+", "símbolo", "MNQ\t"} {
			cfg := config()
			cfg.Instrument = market.Instrument{Symbol: symbol, CentsPerTick: 50}
			if _, err := session.New(cfg, 1_000, nil); !errors.Is(err, market.ErrIdentifierCharacter) {
				t.Fatalf("%q: got %v, want %v", symbol, err, market.ErrIdentifierCharacter)
			}
		}
	})

	t.Run("the name the record uses for absent", func(t *testing.T) {
		// "-" is what the format writes for a level that is not set. A thing
		// actually called "-" would be indistinguishable from nothing, and the
		// reader would blame whichever field went missing rather than the name
		// that caused it.
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
		o := market.Order{
			ID: "-", Instrument: mnq, Side: market.SideBuy,
			Type: market.OrderTypeMarket, Qty: 1,
		}
		if err := s.SubmitOrder(o, decidedAt()); !errors.Is(err, market.ErrReservedIdentifier) {
			t.Fatalf("got %v, want %v", err, market.ErrReservedIdentifier)
		}
		// It is only the whole name that is reserved; a dash inside one is fine.
		mustSubmit(t, s, order("o-1", market.SideBuy, 1))
		checked(t, s)
	})
}

// Scenario: a decision is wholly present or wholly absent
//
// A segment of zero means nobody was there. A stamp carrying a moment in the
// world and no segment would read as an absence while plainly recording that
// somebody acted — and the half that survived is the half no interval can be
// computed from, which is the half the hypothesis needs.
func TestADecisionIsWhollyPresentOrWhollyAbsent(t *testing.T) {
	half := []session.Decision{
		// A moment, an interval or a gesture with no segment to place it in.
		{AtUTCNanos: 1_764_000_000_000_000_000},
		{ElapsedNanos: 40_000_000_000},
		{GestureID: "g-1"},
		// And a segment missing one of the three things a decision needs.
		{Segment: 1, ElapsedNanos: 0, GestureID: "g-1"},
		{Segment: 1, AtUTCNanos: 1_764_000_000_000_000_000},
		{Segment: 1, AtUTCNanos: 1_764_000_000_000_000_000, GestureID: "g-1", ElapsedNanos: -1},
	}

	t.Run("the door refuses it", func(t *testing.T) {
		for _, d := range half {
			s := newSession(t)
			mustOpen(t, s, 2_000, "d1")
			mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
			before := s.JournalLen()
			if err := s.SubmitOrder(order("o1", market.SideBuy, 1), d); !errors.Is(err, session.ErrMalformedDecision) {
				t.Fatalf("%+v: got %v, want %v", d, err, session.ErrMalformedDecision)
			}
			if s.JournalLen() != before || s.NeedsRecovery() != nil {
				t.Fatal("a refused decision recorded something, or stopped the session")
			}
		}
	})

	t.Run("and so do the readers", func(t *testing.T) {
		// The door is not the only guard, because a journal can arrive from
		// somewhere the door never stood.
		for _, d := range half {
			s := newSession(t)
			mustOpen(t, s, 2_000, "d1")
			mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
			mustSubmit(t, s, order("o1", market.SideBuy, 1))

			events := s.Events()
			at := indexOfKind(t, events, session.KindOrderSubmitted, 1)
			submitted := events[at].(session.OrderSubmitted)
			submitted.Decided = d
			events[at] = submitted

			_, err := session.Replay(events)
			if !errors.Is(err, session.ErrStructure) {
				t.Fatalf("%+v: got %v, want %v", d, err, session.ErrStructure)
			}
			if !strings.Contains(err.Error(), "not the rest of it") {
				t.Fatalf("%+v: rejected for another reason: %v", d, err)
			}
		}
	})
}

// Scenario: a gesture is spent once, whatever it commanded
//
// The order identifier makes a repeated submission idempotent and nothing else.
// A lost response to a replacement, a withdrawal or a cancellation, resent by
// the browser, would otherwise arrive as a second human decision — and the log
// exists to hold decisions, so an extra one is not a duplicate record but a
// falsified finding.
//
// The gesture names the act rather than the thing acted on, so every human
// command is covered by one rule. The set is reconstructed from the journal
// rather than held in a server's memory, which is what makes it survive a
// restart.
func TestAGestureIsSpentOnceWhateverItCommanded(t *testing.T) {
	t.Run("a replacement resent", func(t *testing.T) {
		s := protectedSession(t, 18_900, 19_500)
		act := decided(0)
		if err := s.ReplaceProtection(entryRef("entry"), 18_800, 19_500, act); err != nil {
			t.Fatalf("ReplaceProtection: %v", err)
		}
		before := s.JournalLen()
		if err := s.ReplaceProtection(entryRef("entry"), 18_700, 19_500, act); !errors.Is(err, session.ErrGestureReused) {
			t.Fatalf("error: got %v, want %v", err, session.ErrGestureReused)
		}
		if s.JournalLen() != before || onlyPlanned(t, s).StopPrice != 18_800 {
			t.Fatal("a resent gesture changed the protection")
		}
		checked(t, s)
	})

	t.Run("a withdrawal resent", func(t *testing.T) {
		s := protectedSession(t, 18_900, 19_500)
		act := decided(0)
		if err := s.CancelProtection(entryRef("entry"), act); err != nil {
			t.Fatalf("CancelProtection: %v", err)
		}
		if err := s.CancelProtection(entryRef("entry"), act); !errors.Is(err, session.ErrGestureReused) {
			t.Fatalf("error: got %v, want %v", err, session.ErrGestureReused)
		}
		checked(t, s)
	})

	t.Run("a cancellation resent", func(t *testing.T) {
		s := protectedSession(t, 18_900, 19_500)
		act := decided(0)
		if err := s.CancelOrder("entry", act); err != nil {
			t.Fatalf("CancelOrder: %v", err)
		}
		if err := s.CancelOrder("entry", act); !errors.Is(err, session.ErrGestureReused) {
			t.Fatalf("error: got %v, want %v", err, session.ErrGestureReused)
		}
		checked(t, s)
	})

	t.Run("a gesture the record cannot write is refused", func(t *testing.T) {
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
		bad := decided(0)
		bad.GestureID = "gesture 1"
		if err := s.SubmitOrder(order("o1", market.SideBuy, 1), bad); !errors.Is(err, market.ErrIdentifierCharacter) {
			t.Fatalf("error: got %v, want %v", err, market.ErrIdentifierCharacter)
		}
	})

	t.Run("and a journal claiming one act twice is refused", func(t *testing.T) {
		// The door is not the only guard: a journal can arrive from somewhere
		// the door never stood, and two decisions under one act would mean the
		// analysis counted a retry as a second thing the person did.
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
		mustSubmit(t, s, order("o1", market.SideBuy, 1))
		mustSubmit(t, s, order("o2", market.SideBuy, 1))

		events := s.Events()
		first := events[indexOfKind(t, events, session.KindOrderSubmitted, 1)].(session.OrderSubmitted)
		at := indexOfKind(t, events, session.KindOrderSubmitted, 2)
		second := events[at].(session.OrderSubmitted)
		second.Decided.GestureID = first.Decided.GestureID
		events[at] = second

		if _, err := session.Replay(events); !errors.Is(err, session.ErrFabricated) {
			t.Fatalf("Replay: got %v, want %v", err, session.ErrFabricated)
		}
		if err := session.Verify(events); !errors.Is(err, session.ErrContradictoryLog) {
			t.Fatalf("Verify: got %v, want %v", err, session.ErrContradictoryLog)
		}
	})

	t.Run("and it survives a restart", func(t *testing.T) {
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
		act := decided(0)
		if err := s.SubmitOrder(order("o1", market.SideBuy, 1), act); err != nil {
			t.Fatalf("SubmitOrder: %v", err)
		}

		state, err := session.Replay(s.Events())
		if err != nil {
			t.Fatalf("Replay: %v", err)
		}
		resumed, err := session.Resume(state, nil)
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		// The browser retries after the restart, under the same act.
		if err := resumed.SubmitOrder(order("o2", market.SideBuy, 1), act); !errors.Is(err, session.ErrGestureReused) {
			t.Fatalf("error: got %v, want %v", err, session.ErrGestureReused)
		}
	})
}

// Scenario: the pacing and the subject are one claim
//
// A pilot or confirmatory session is somebody's by definition and a scripted
// run is nobody's. A configuration that says how observations reached a person
// without saying which person, or names a person and says nothing was presented
// to them, asserts two incompatible things — and the dangerous half is a
// fixture labelled as though a person had traded it, which is the one thing the
// pilot sample must never contain.
func TestThePacingAndTheSubjectAreOneClaim(t *testing.T) {
	refused := []struct {
		name    string
		subject string
		pacing  session.PacingMode
	}{
		{"presented to nobody", "", session.PacingPilot},
		{"confirmatory for nobody", "", session.PacingConfirmatory},
		{"a subject nothing was presented to", "t-01", session.PacingScripted},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config()
			cfg.SubjectID, cfg.Pacing = tc.subject, tc.pacing
			if _, err := session.New(cfg, 1_000, nil); !errors.Is(err, session.ErrPacingWithoutSubject) {
				t.Fatalf("got %v, want %v", err, session.ErrPacingWithoutSubject)
			}
		})
	}

	// And a reader refuses it too, because a journal can arrive from somewhere
	// the door never stood.
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	events := s.Events()
	started := events[0].(session.SessionStarted)
	started.Config.Pacing = session.PacingScripted
	events[0] = started
	if _, err := session.Replay(events); !errors.Is(err, session.ErrStructure) {
		t.Fatalf("Replay: got %v, want %v", err, session.ErrStructure)
	}
}

// Scenario: a retry is answered from the journal, not from a server's memory
//
// A set of spent names says an act happened; it does not say what it did, so it
// cannot tell a resent command from a different command sent under a name
// reused by mistake. The application loop needs both, and it needs them to
// survive a restart — the client's counter prevents accidental reuse, but the
// journal is the authority on what was confirmed.
func TestWhatAGestureCommandedIsRecoverable(t *testing.T) {
	s := protectedSession(t, 18_900, 19_500)
	replace := decided(0)
	if err := s.ReplaceProtection(entryRef("entry"), 18_800, 19_500, replace); err != nil {
		t.Fatalf("ReplaceProtection: %v", err)
	}

	state, err := session.Replay(s.Events())
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if !reflect.DeepEqual(state.Gestures, s.Gestures()) {
		t.Fatalf("acts\n got: %+v\nwant: %+v", state.Gestures, s.Gestures())
	}
	resumed, err := session.Resume(state, nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !reflect.DeepEqual(resumed.Gestures(), s.Gestures()) {
		t.Fatal("a resumed session forgot what its acts commanded")
	}

	acts := resumed.Gestures()
	if len(acts) != 2 {
		t.Fatalf("acts: got %d, want the submission and the replacement", len(acts))
	}

	// The submission carries the levels placed with it: an entry and its
	// protection are one act, so resending the entry with different levels is
	// a different command rather than a retry.
	submit := acts[0]
	if submit.Kind != session.GestureSubmitOrder || submit.StopPrice != 18_900 || submit.TargetPrice != 19_500 {
		t.Fatalf("the submission: got %+v", submit)
	}
	// And the replacement carries what it asked for.
	change := acts[1]
	if change.Kind != session.GestureReplaceProtection || change.StopPrice != 18_800 {
		t.Fatalf("the replacement: got %+v", change)
	}
	if change.Decided != replace {
		t.Fatalf("the stamp was remade rather than recovered: got %+v, want %+v", change.Decided, replace)
	}

	// A retry is the same command at a later instant, so the stamp is not part
	// of the comparison — comparing it would make every retry a conflict.
	retry := change
	retry.Decided = decided(90_000_000_000)
	if !change.SameCommand(retry) {
		t.Fatal("the same command at a later moment did not compare equal")
	}
	// A different command under the same name is a conflict, not a retry.
	other := change
	other.StopPrice = 18_700
	if change.SameCommand(other) {
		t.Fatal("a different command compared equal")
	}
}

// Scenario: two acts are the same command only if they asked for the same thing
//
// This is the comparison the application loop answers a retry with, so every
// field it ignores is a way for a different command to be mistaken for a resend
// — and a resend that is not one is a decision the person never took, recorded
// as though they had.
func TestTwoActsAreTheSameCommandOnlyIfTheyAskedTheSame(t *testing.T) {
	entry := order("o1", market.SideBuy, 2)
	base := map[session.GestureKind]session.Gesture{
		session.GestureSubmitOrder: {
			Kind: session.GestureSubmitOrder, Order: entry,
			StopPrice: 19_900, TargetPrice: 20_400,
		},
		session.GestureCancelOrder: {Kind: session.GestureCancelOrder, OrderID: "o1"},
		session.GestureReplaceProtection: {
			Kind: session.GestureReplaceProtection, Ref: entryRef("o1"),
			StopPrice: 19_800, TargetPrice: 20_400,
		},
		session.GestureWithdrawProtection: {
			Kind: session.GestureWithdrawProtection, Ref: entryRef("o1"),
		},
	}

	differs := []struct {
		name  string
		kind  session.GestureKind
		alter func(*session.Gesture)
	}{
		{"a submission of another order", session.GestureSubmitOrder,
			func(g *session.Gesture) { g.Order = order("o2", market.SideBuy, 2) }},
		{"a submission of another quantity", session.GestureSubmitOrder,
			func(g *session.Gesture) { g.Order = order("o1", market.SideBuy, 3) }},
		{"a submission with another stop", session.GestureSubmitOrder,
			func(g *session.Gesture) { g.StopPrice = 19_700 }},
		{"a submission with another target", session.GestureSubmitOrder,
			func(g *session.Gesture) { g.TargetPrice = 20_500 }},
		{"a cancellation of another order", session.GestureCancelOrder,
			func(g *session.Gesture) { g.OrderID = "o2" }},
		{"a replacement of another protection", session.GestureReplaceProtection,
			func(g *session.Gesture) { g.Ref = entryRef("o2") }},
		{"a replacement to another stop", session.GestureReplaceProtection,
			func(g *session.Gesture) { g.StopPrice = 19_700 }},
		{"a replacement to another target", session.GestureReplaceProtection,
			func(g *session.Gesture) { g.TargetPrice = 20_500 }},
		{"a withdrawal of another protection", session.GestureWithdrawProtection,
			func(g *session.Gesture) { g.Ref = entryRef("o2") }},
	}

	for _, tc := range differs {
		t.Run(tc.name, func(t *testing.T) {
			original := base[tc.kind]
			other := original
			tc.alter(&other)
			if original.SameCommand(other) {
				t.Fatalf("a different command compared equal:\n %+v\n %+v", original, other)
			}
			if !original.SameCommand(original) {
				t.Fatal("a command did not compare equal to itself")
			}
		})
	}

	// A different kind is a different command whatever else matches, and the
	// stamp is never part of it: a retry is the same command at a later
	// instant, which is the case the comparison exists to recognise.
	for kind, act := range base {
		for otherKind, other := range base {
			if kind == otherKind {
				continue
			}
			if act.SameCommand(other) {
				t.Fatalf("%v compared equal to %v", kind, otherKind)
			}
		}
		later := act
		later.Decided = decided(90_000_000_000)
		if !act.SameCommand(later) {
			t.Fatalf("%v did not compare equal to itself at a later moment", kind)
		}
	}
}

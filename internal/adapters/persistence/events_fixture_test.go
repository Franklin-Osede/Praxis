package persistence_test

import (
	"testing"

	"praxis/internal/adapters/persistence"
	"praxis/internal/challenge"
	"praxis/internal/market"
	"praxis/internal/portfolio"
	"praxis/internal/session"
)

var mnq = market.Instrument{Symbol: "MNQ", CentsPerTick: 50}

// humanAt is a stand-in for a person's clock, which the adapter supplies and
// the kernel only records.
const humanAt = market.WallClock(1_764_000_000_000_000_000)

// everyEventTypeV2 is everyEventType plus the one thing only v2 added: a
// decision carrying a losing-trade streak. The protection events that were
// declared in v2 were never written by anything and could not be — see
// ADR-014 — so they are not part of it.
func everyEventTypeV2() []session.Event {
	events := everyEventType()
	for n, e := range events {
		if o, ok := e.(session.OrderSubmitted); ok {
			o.Context.ConsecutiveLosingTrades = 3
			events[n] = o
		}
	}
	return events
}

// everyEventTypeV3 adds protection, whose shape ADR-014 settled and whose
// producer exists in the same change that publishes these bytes.
func everyEventTypeV3() []session.Event {
	return append(everyEventTypeV2(),
		session.ProtectionPlaced{
			Envelope:     session.Envelope{Time: 9_000, Sequence: 12, Kind: session.KindProtectionPlaced},
			EntryOrderID: "o-4", StopPrice: 19_900, TargetPrice: 20_400,
			StopOrderID: "praxis:12:stop", TargetOrderID: "praxis:12:target",
		},
		session.ProtectionReplaced{
			Envelope:          session.Envelope{Time: 9_000, Sequence: 13, Kind: session.KindProtectionReplaced},
			Ref:               session.ProtectionRef{Kind: session.ProtectionRefEntry, OrderID: "o-4"},
			PreviousStopPrice: 19_900, PreviousTargetPrice: 20_400,
			StopPrice: 19_800, TargetPrice: 0,
			StopOrderID: "praxis:12:stop", TargetOrderID: "",
			Widened: true,
		},
		session.ProtectionEnded{
			Envelope:  session.Envelope{Time: 9_000, Sequence: 14, Kind: session.KindProtectionEnded},
			Ref:       session.ProtectionRef{Kind: session.ProtectionRefEpisode, EpisodeID: 10},
			StopPrice: 19_800, Reason: session.ProtectionWithdrawnByTrader,
		},
	)
}

// everyEventTypeV4 adds the cancellation reasons one-cancels-the-other
// execution needs, and who traded the journal. A reason is a value and a
// subject is a field, and an older reader has no name for either — which is
// why both take a version of their own, and why the subject went in while v4
// was still a draft rather than after the first recorded session.
func everyEventTypeV4() []session.Event {
	events := everyEventTypeV3()
	// Who traded it, and when they acted by their own clock. Both are fields
	// on lines older versions already had, which is why both are v4.
	for n, e := range events {
		switch v := e.(type) {
		case session.SessionStarted:
			v.Config.SubjectID = "s-07"
			events[n] = v
		case session.OrderSubmitted:
			v.DecidedAt = humanAt
			events[n] = v
		case session.ProtectionReplaced:
			v.DecidedAt = humanAt
			events[n] = v
		}
	}
	return append(events,
		session.OrderCancelled{
			Envelope: session.Envelope{Time: 9_000, Sequence: 15, Kind: session.KindOrderCancelled},
			OrderID:  "praxis:12:target", RemainingQty: 7, Reason: session.CancelledByOCO,
		},
		session.OrderCancelled{
			Envelope: session.Envelope{Time: 9_000, Sequence: 16, Kind: session.KindOrderCancelled},
			OrderID:  "praxis:12:stop", RemainingQty: 10, Reason: session.CancelledPositionClosed,
		},
	)
}

// everyEventType is one instance of each of the nine events, with every field
// set to a distinguishable value so a golden file pins each one's position.
func everyEventType() []session.Event {
	return []session.Event{
		session.SessionStarted{
			Envelope: session.Envelope{Time: 1_000, Sequence: 1, Kind: session.KindSessionStarted},
			Config: session.Config{
				Instrument:               mnq,
				StartingBalanceCts:       5_000_000,
				CommissionPerContractCts: 50,
				Rules: challenge.Rules{
					StartingBalanceCts:  5_000_000,
					MaxDailyLossCts:     100_000,
					ProfitTargetCts:     300_000,
					MaxTotalLossCts:     200_000,
					TrailingDrawdownCts: 250_000,
				},
			},
		},
		session.SessionOpened{
			Envelope:  session.Envelope{Time: 2_000, Sequence: 2, Kind: session.KindSessionOpened},
			SessionID: "2026-08-27", BalanceCts: 5_000_000, EquityCts: 5_000_000,
		},
		session.ChallengeDecision{
			Envelope:         session.Envelope{Time: 2_000, Sequence: 3, Kind: session.KindChallengeDecision},
			CausedBySequence: 2,
			Decision: challenge.Decision{
				Kind: challenge.ChallengeActivated, SessionID: "2026-08-27",
				BalanceCts: 5_000_000, EquityCts: 5_000_000,
			},
		},
		session.AccountValued{
			Envelope:  session.Envelope{Time: 2_000, Sequence: 4, Kind: session.KindAccountValued},
			SessionID: "2026-08-27", BalanceCts: 5_000_000, EquityCts: 4_999_900,
		},
		session.MarketObserved{
			Envelope: session.Envelope{Time: 3_000, Sequence: 5, Kind: session.KindMarketObserved},
			Quote: market.Quote{
				Instrument: mnq, Time: 3_000,
				Bid: 20_000, Ask: 20_001, BidSize: 10, AskSize: 12,
			},
			SourceSequence: 4,
		},
		session.OrderSubmitted{
			Envelope: session.Envelope{Time: 3_000, Sequence: 6, Kind: session.KindOrderSubmitted},
			Order: market.Order{
				ID: "o-1", Instrument: mnq, Side: market.SideSell,
				Type: market.OrderTypeLimit, Qty: 3, LimitPrice: 20_050,
			},
			Context: session.OrderContext{
				BalanceCts: 5_000_000, EquityCts: 4_999_900,
				OrdersSubmittedThisSession: 2, ConsecutiveLosses: 1,
				SessionRealisedCts: -1_500, PositionQtyBefore: -4,
			},
		},
		session.OrderRested{
			Envelope: session.Envelope{Time: 3_000, Sequence: 7, Kind: session.KindOrderRested},
			Order: market.Order{
				ID: "o-2", Instrument: mnq, Side: market.SideBuy,
				Type: market.OrderTypeStop, Qty: 5, StopPrice: 20_060,
			},
			RestingQty: 4,
		},
		session.OrderCancelled{
			Envelope: session.Envelope{Time: 3_000, Sequence: 8, Kind: session.KindOrderCancelled},
			OrderID:  "o-3", RemainingQty: 6, Reason: session.CancelledUnfillableRemainder,
		},
		session.FillProduced{
			Envelope: session.Envelope{Time: 3_000, Sequence: 9, Kind: session.KindFillProduced},
			Fill: market.Fill{
				OrderID: "o-1", Instrument: mnq, Time: 3_000,
				Side: market.SideSell, Price: 20_050, Qty: 3,
			},
		},
		session.PositionChanged{
			Envelope: session.Envelope{Time: 3_000, Sequence: 10, Kind: session.KindPositionChanged},
			Change: portfolio.PositionEvent{
				Kind: portfolio.PositionReduced, Instrument: mnq, Side: market.SideSell,
				Qty: 3, Price: 20_050, RealisedCts: -750, FeeCts: 150,
			},
		},
		session.SessionEnded{
			Envelope:  session.Envelope{Time: 9_000, Sequence: 11, Kind: session.KindSessionEnded},
			SessionID: "2026-08-27",
		},
	}
}

func sessionIDOf(n int) challenge.SessionID {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return challenge.SessionID(b)
}

// realSessionEvents is a journal a session actually produced, rather than a
// hand-built one: the codec must survive what the system emits, not only what
// a fixture remembered to include.
func realSessionEvents(t *testing.T) []session.Event {
	t.Helper()
	cfg := session.Config{
		Instrument:               mnq,
		StartingBalanceCts:       5_000_000,
		CommissionPerContractCts: 50,
		Rules: challenge.Rules{
			StartingBalanceCts: 5_000_000,
			MaxDailyLossCts:    100_000,
			ProfitTargetCts:    100_000,
			MaxTotalLossCts:    200_000,
		},
	}
	s, err := session.New(cfg, 1_000, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.OpenTradingSession(2_000, "d1"); err != nil {
		t.Fatalf("OpenTradingSession: %v", err)
	}
	q := market.Quote{Instrument: mnq, Time: 3_000, Bid: 20_000, Ask: 20_001, BidSize: 50, AskSize: 50}
	if err := s.Observe(q, 1); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	buy, err := market.NewMarketOrder("o-1", mnq, market.SideBuy, 3)
	if err != nil {
		t.Fatalf("NewMarketOrder: %v", err)
	}
	if err := s.SubmitOrder(buy, humanAt); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
	q2 := market.Quote{Instrument: mnq, Time: 4_000, Bid: 19_990, Ask: 19_991, BidSize: 50, AskSize: 50}
	if err := s.Observe(q2, 1); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	sell, err := market.NewMarketOrder("o-2", mnq, market.SideSell, 3)
	if err != nil {
		t.Fatalf("NewMarketOrder: %v", err)
	}
	if err := s.SubmitOrder(sell, humanAt); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
	if err := s.EndTradingSession(5_000); err != nil {
		t.Fatalf("EndTradingSession: %v", err)
	}
	return s.Events()
}

// journalBytes frames a set of events as one batch inside a complete file.
func journalBytes(t *testing.T, batches ...[]session.Event) []byte {
	t.Helper()
	out := persistence.Header()
	for n, events := range batches {
		framed, err := persistence.EncodeBatch(uint64(n+1), events, persistence.EventVersion)
		if err != nil {
			t.Fatalf("EncodeBatch: %v", err)
		}
		out = append(out, framed...)
	}
	return out
}

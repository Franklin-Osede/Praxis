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
	if err := s.SubmitOrder(buy); err != nil {
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
	if err := s.SubmitOrder(sell); err != nil {
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
		framed, err := persistence.EncodeBatch(uint64(n+1), events)
		if err != nil {
			t.Fatalf("EncodeBatch: %v", err)
		}
		out = append(out, framed...)
	}
	return out
}

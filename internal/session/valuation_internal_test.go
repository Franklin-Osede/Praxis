package session

import (
	"errors"
	"testing"

	"praxis/internal/challenge"
	"praxis/internal/market"
)

// Scenario: a valuation that cannot be taken is reported, not answered
//
//	Given a session holding a position and no book to mark it at
//	Then Valuation returns the failure rather than a figure.
//
// The adapter above it turns that into a terminal screen. If this swallowed the
// error and answered anyway, the interface would have nothing to notice: the
// figure would look like every other figure.
func TestValuationReportsWhatItCannotTake(t *testing.T) {
	instrument := market.Instrument{Symbol: "MNQ", CentsPerTick: 50}
	s, err := New(Config{
		Instrument: instrument, SubjectID: "t-01", Pacing: PacingPilot,
		StartingBalanceCts: 5_000_000, CommissionPerContractCts: 50,
		Rules: challenge.Rules{
			StartingBalanceCts: 5_000_000, MaxDailyLossCts: 100_000,
			ProfitTargetCts: 100_000, MaxTotalLossCts: 200_000,
		},
	}, 1_000, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Valuation(); err != nil {
		t.Fatalf("a flat account could not be valued: %v", err)
	}

	// A position with no observation behind it: reachable only from here, and
	// exactly the shape a resumed state could carry.
	if _, err := s.account.ApplyFill(market.Fill{
		OrderID: "o-1", Instrument: instrument, Time: 2_000,
		Side: market.SideBuy, Price: 20_000, Qty: 1,
	}); err != nil {
		t.Fatalf("ApplyFill: %v", err)
	}
	if _, err := s.Valuation(); !errors.Is(err, ErrNoMarketObserved) {
		t.Fatalf("Valuation: got %v, want %v", err, ErrNoMarketObserved)
	}
}

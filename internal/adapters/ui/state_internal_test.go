package ui

import (
	"testing"

	"praxis/internal/challenge"
	"praxis/internal/market"
	"praxis/internal/session"
)

// Scenario: the screen never says a trading session is open after it ended
//
//	Given a session whose trading session has closed
//	Then the state the participant is served reports none open, and names none.
//
// Openness was read from a non-empty identifier, and the identifier used to
// outlive the session. The participant would have been shown a session running
// that was over — and what the screen says is the claim OrderContext makes
// about what they knew.
func TestTheStateReportsNoSessionAfterOneEnds(t *testing.T) {
	instrument := market.Instrument{Symbol: "MNQ", CentsPerTick: 50}
	cfg := session.Config{
		Instrument: instrument, SubjectID: "t-01", Pacing: session.PacingPilot,
		StartingBalanceCts: 5_000_000, CommissionPerContractCts: 50,
		Rules: challenge.Rules{
			StartingBalanceCts: 5_000_000, MaxDailyLossCts: 100_000,
			ProfitTargetCts: 100_000, MaxTotalLossCts: 200_000,
		},
	}
	s, err := session.New(cfg, 1_000, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.OpenTradingSession(2_000, challenge.SessionID("d1")); err != nil {
		t.Fatalf("OpenTradingSession: %v", err)
	}
	if got := project(s, 0, 1, cfg, 5_000_000, 5_000_000); !got.SessionOpen || got.SessionID != "d1" {
		t.Fatalf("while open: got open=%v id=%q, want true and d1", got.SessionOpen, got.SessionID)
	}

	if err := s.EndTradingSession(3_000); err != nil {
		t.Fatalf("EndTradingSession: %v", err)
	}
	got := project(s, 0, 1, cfg, 5_000_000, 5_000_000)
	if got.SessionOpen {
		t.Fatal("the screen reports a trading session open after it ended")
	}
	if got.SessionID != "" {
		t.Fatalf("the screen names %q as the open session after it ended", got.SessionID)
	}
}

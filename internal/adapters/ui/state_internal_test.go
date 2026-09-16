package ui

import (
	"errors"
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
		Instrument: instrument, SubjectID: "t-01", RunID: "r-01", Pacing: session.PacingPilot,
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
	if got := project(s, 0, 1, cfg, session.Valuation{BalanceCts: 5_000_000, EquityCts: 5_000_000}, nil); !got.SessionOpen || got.SessionID != "d1" {
		t.Fatalf("while open: got open=%v id=%q, want true and d1", got.SessionOpen, got.SessionID)
	}

	if err := s.EndTradingSession(3_000); err != nil {
		t.Fatalf("EndTradingSession: %v", err)
	}
	got := project(s, 0, 1, cfg, session.Valuation{BalanceCts: 5_000_000, EquityCts: 5_000_000}, nil)
	if got.SessionOpen {
		t.Fatal("the screen reports a trading session open after it ended")
	}
	if got.SessionID != "" {
		t.Fatalf("the screen names %q as the open session after it ended", got.SessionID)
	}
}

// Scenario: a valuation that could not be taken is not replaced by another one
//
//	Given a valuation that failed
//	Then the screen carries no money at all and says the session has stopped.
//
// The figure it used to fall back to was the balance, which for a participant
// holding a losing position is money they do not have — and it is a number they
// act on. An interface may show nothing; it may not show something else.
func TestAFailedValuationIsTerminalOnTheScreen(t *testing.T) {
	instrument := market.Instrument{Symbol: "MNQ", CentsPerTick: 50}
	cfg := session.Config{
		Instrument: instrument, SubjectID: "t-01", RunID: "r-01", Pacing: session.PacingPilot,
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

	got := project(s, 0, 1, cfg, session.Valuation{}, errors.New("no mark for an open position"))
	// Absent, not empty: an empty decimal is not a decimal, and a client
	// parsing one would paint NaN next to the notice.
	if got.Money != nil {
		t.Fatalf("money survived a failed valuation: %+v", *got.Money)
	}
	if got.NeedsRecovery == "" {
		t.Fatal("a failed valuation is not reported")
	}
}

// Scenario: a stopped session is the more fundamental of the two
//
//	Given a session that has stopped and a valuation that also failed
//	Then the screen reports the stopped session.
//
// NeedsRecovery carries two facts and only one can be shown. A session that
// will accept nothing further is what an operator acts on; a valuation failing
// inside a dead session is a consequence of it, and reporting that instead
// would send them looking at the wrong thing.
func TestAStoppedSessionOutranksAFailedValuation(t *testing.T) {
	instrument := market.Instrument{Symbol: "MNQ", CentsPerTick: 50}
	cfg := session.Config{
		Instrument: instrument, SubjectID: "t-01", RunID: "r-01", Pacing: session.PacingPilot,
		StartingBalanceCts: 5_000_000, CommissionPerContractCts: 50,
		Rules: challenge.Rules{
			StartingBalanceCts: 5_000_000, MaxDailyLossCts: 100_000,
			ProfitTargetCts: 100_000, MaxTotalLossCts: 200_000,
		},
	}
	s, err := session.New(cfg, 1_000, refusingCommitter{})
	if err == nil {
		t.Fatal("the fixture did not stop the session")
	}
	if s != nil {
		t.Fatal("a session that could not record its start was returned")
	}

	// A session that stopped after starting: the commit of its first trading
	// session is what fails.
	accepting := &firstOnly{}
	s, err = session.New(cfg, 1_000, accepting)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.OpenTradingSession(2_000, challenge.SessionID("d1")); err == nil {
		t.Fatal("the fixture did not stop the session")
	}
	if s.NeedsRecovery() == nil {
		t.Fatal("the session did not stop")
	}

	got := project(s, 0, 1, cfg, session.Valuation{}, errors.New("a valuation that also failed"))
	if got.NeedsRecovery != s.NeedsRecovery().Error() {
		t.Fatalf("needsRecovery: got %q, want the stopped session %q",
			got.NeedsRecovery, s.NeedsRecovery())
	}
}

// refusingCommitter refuses everything.
type refusingCommitter struct{}

func (refusingCommitter) Commit([]session.Event) error { return errors.New("refused") }

// firstOnly takes the session's start and refuses what follows.
type firstOnly struct{ taken int }

func (c *firstOnly) Commit([]session.Event) error {
	c.taken++
	if c.taken > 1 {
		return errors.New("the disk went away")
	}
	return nil
}

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

// acknowledged keeps the interaction clock moving forward across the helpers.
var acknowledged int64

// screenSession is a pilot session with an observation on the screen, ready for
// a command.
func screenSession(t *testing.T, bid, ask market.Ticks, size market.Qty) (*session.Session, session.Config) {
	t.Helper()
	instrument := market.Instrument{Symbol: "MNQ", CentsPerTick: 50}
	cfg := session.Config{
		Instrument: instrument, SubjectID: "t-01", RunID: "r-01", Pacing: session.PacingPilot,
		StartingBalanceCts: 5_000_000, CommissionPerContractCts: 50,
		Rules: challenge.Rules{
			StartingBalanceCts: 5_000_000, MaxDailyLossCts: 1_000_000,
			ProfitTargetCts: 1_000_000, MaxTotalLossCts: 2_000_000,
		},
	}
	s, err := session.New(cfg, 1_000, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.OpenTradingSession(2_000, challenge.SessionID("d1")); err != nil {
		t.Fatalf("OpenTradingSession: %v", err)
	}
	observe(t, s, 3_000, bid, ask, size)
	return s, cfg
}

func observe(t *testing.T, s *session.Session, at market.LogicalTime, bid, ask market.Ticks, size market.Qty) {
	t.Helper()
	q := market.Quote{Instrument: market.Instrument{Symbol: "MNQ", CentsPerTick: 50},
		Time: at, Bid: bid, Ask: ask, BidSize: size, AskSize: size}
	if err := s.Observe(q, 1); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	id, waiting := s.Pending(1)
	if !waiting {
		t.Fatal("nothing to present")
	}
	acknowledged++
	if err := s.AcknowledgePresentation(id, session.Instant{
		Segment: 1, AtUTCNanos: 1_764_000_000_000_000_000,
		ElapsedNanos: session.ElapsedNanos(acknowledged * 1_000),
	}); err != nil {
		t.Fatalf("AcknowledgePresentation: %v", err)
	}
}

func submit(t *testing.T, s *session.Session, id string, side market.Side, qty market.Qty, gesture string) {
	t.Helper()
	o, err := market.NewMarketOrder(id, market.Instrument{Symbol: "MNQ", CentsPerTick: 50}, side, qty)
	if err != nil {
		t.Fatalf("NewMarketOrder: %v", err)
	}
	acknowledged++
	if err := s.SubmitOrder(o, session.Decision{
		GestureID: gesture, Segment: 1, AtUTCNanos: 1_764_000_000_000_000_001,
		ElapsedNanos: session.ElapsedNanos(acknowledged * 1_000),
	}); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
}

// Scenario: a reversal is reported as both legs, not as a close
//
//	Given a long of one
//	When the participant sells three on one order
//	Then the screen says the fill closed one and opened two short.
//
// One fill, two position changes. Reporting the first left a participant
// reading "position closed" beside a position row showing short two — the line
// and the numbers contradicting each other, which is the confound this field
// exists to remove rather than another instance of it.
func TestTheScreenReportsBothLegsOfAReversal(t *testing.T) {
	s, cfg := screenSession(t, 20_000, 20_001, 50)
	submit(t, s, "o-1", market.SideBuy, 1, "g-1")
	submit(t, s, "o-2", market.SideSell, 3, "g-2")

	fills := project(s, 1, 1, cfg, session.Valuation{}, nil).Fills
	if len(fills) != 2 {
		t.Fatalf("the screen names %d fills, want the entry and the reversal: %+v", len(fills), fills)
	}
	reversal := fills[1]
	if reversal.Cause != "order" || reversal.Side != "sell" || reversal.Qty != "3" {
		t.Fatalf("the reversal reads %+v", reversal)
	}
	if len(reversal.Changes) != 2 {
		t.Fatalf("the reversal reports %+v, want both legs", reversal.Changes)
	}
	if reversal.Changes[0].Kind != "closed" || reversal.Changes[0].Qty != "1" {
		t.Fatalf("the first leg reads %+v, want closed 1", reversal.Changes[0])
	}
	if reversal.Changes[1].Kind != "opened" || reversal.Changes[1].Qty != "2" {
		t.Fatalf("the second leg reads %+v, want opened 2", reversal.Changes[1])
	}
}

// Scenario: two fills on the row on the screen are both named
//
// A working order and another filling on the same observation is one screen and
// two facts. Naming one of them tells the participant something true about a
// smaller event than the one that happened.
func TestTheScreenNamesEveryFillOnTheRow(t *testing.T) {
	s, cfg := screenSession(t, 20_000, 20_001, 50)
	submit(t, s, "o-1", market.SideBuy, 1, "g-1")
	submit(t, s, "o-2", market.SideBuy, 2, "g-2")

	fills := project(s, 1, 1, cfg, session.Valuation{}, nil).Fills
	if len(fills) != 2 {
		t.Fatalf("the screen names %d fills, want both: %+v", len(fills), fills)
	}
	if fills[0].Qty != "1" || fills[1].Qty != "2" {
		t.Fatalf("the fills read %+v, want 1 then 2", fills)
	}
}

// Scenario: the fills belong to the row on the screen, and go when it does
//
// "The last fill" outlives its observation and would sit on the screen through
// rows it has nothing to do with.
func TestTheFillsBelongToTheRowOnTheScreen(t *testing.T) {
	s, cfg := screenSession(t, 20_000, 20_001, 50)
	submit(t, s, "o-1", market.SideBuy, 1, "g-1")
	if got := project(s, 1, 2, cfg, session.Valuation{}, nil).Fills; len(got) != 1 {
		t.Fatalf("on the row it happened on: %+v", got)
	}

	observe(t, s, 4_000, 20_010, 20_011, 50)
	if got := project(s, 2, 2, cfg, session.Valuation{}, nil).Fills; len(got) != 0 {
		t.Fatalf("after the market moved on: %+v, want none", got)
	}
}

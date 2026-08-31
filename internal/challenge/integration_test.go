package challenge_test

import (
	"testing"

	"praxis/internal/challenge"
	"praxis/internal/market"
	"praxis/internal/portfolio"
)

// These tests exist because a challenge unit test cannot prove them. Challenge
// receives one number, EquityCts, and cannot tell which part of it came from
// unrealised P&L or from commissions; asserting it inside the package would
// only restate its own input. The claim is about how equity is computed, so it
// is proven against a real account.

var mnq = market.Instrument{Symbol: "MNQ", CentsPerTick: 50}

func mustAccount(t *testing.T, startingCts, commissionCts market.Cents) *portfolio.Account {
	t.Helper()
	a, err := portfolio.NewAccount(startingCts, commissionCts)
	if err != nil {
		t.Fatalf("NewAccount: %v", err)
	}
	return a
}

func mustFill(t *testing.T, a *portfolio.Account, side market.Side, qty market.Qty, price market.Ticks) {
	t.Helper()
	if _, err := a.ApplyFill(market.Fill{
		OrderID: "o-1", Instrument: mnq, Time: 1, Side: side, Price: price, Qty: qty,
	}); err != nil {
		t.Fatalf("ApplyFill: %v", err)
	}
}

// valuationAt is the atomic account valuation an adapter would take before
// stamping an input: both figures from the same account at the same mark.
func valuationAt(t *testing.T, a *portfolio.Account, mark market.Ticks) (balanceCts, equityCts market.Cents) {
	t.Helper()
	balanceCts, err := a.BalanceCts()
	if err != nil {
		t.Fatalf("BalanceCts: %v", err)
	}
	equityCts, err = a.EquityCts([]portfolio.Mark{{Instrument: mnq, Price: mark}})
	if err != nil {
		t.Fatalf("EquityCts: %v", err)
	}
	return balanceCts, equityCts
}

func openAccount(t *testing.T, c *challenge.Challenge, seq uint64, id challenge.SessionID, a *portfolio.Account, mark market.Ticks) {
	t.Helper()
	balance, equity := valuationAt(t, a, mark)
	mustOpen(t, c, openedAt(seq, id, balance, equity))
}

func observeAccount(t *testing.T, c *challenge.Challenge, seq uint64, id challenge.SessionID, a *portfolio.Account, mark market.Ticks) {
	t.Helper()
	balance, equity := valuationAt(t, a, mark)
	mustObserve(t, c, snapAt(seq, id, balance, equity))
}

// Scenario: an unrealised loss fails an evaluation with no trade closed
//
//	Given an account holding an open long and a $1,000 daily loss limit
//	When the mark falls far enough that equity breaches the limit
//	Then the challenge fails, although realised P&L is still zero and
//	  nothing has been sold.
func TestUnrealisedLossFailsTheChallenge(t *testing.T) {
	a := mustAccount(t, 5_000_000, 0)
	c := newChallenge(t)

	openAccount(t, c, 1, "day-1", a, 20_000)
	mustFill(t, a, market.SideBuy, 10, 20_000)

	// Down exactly $1,000 on the mark: at the limit, not past it.
	observeAccount(t, c, 2, "day-1", a, 19_800)
	if c.State() != challenge.StateActive {
		t.Fatalf("state: got %v, want active at exactly the limit", c.State())
	}

	// One tick further is $5 more, comfortably past it.
	observeAccount(t, c, 3, "day-1", a, 19_799)
	if c.State() != challenge.StateFailed {
		t.Fatalf("state: got %v, want failed", c.State())
	}

	if a.RealisedCts() != 0 {
		t.Fatalf("realised: got %d, want 0 — the failure must be entirely unrealised", a.RealisedCts())
	}
	if p, _ := a.Position(mnq); p.NetQty != 10 {
		t.Fatalf("position: got %d, want the long still open", p.NetQty)
	}
}

// Scenario: commissions alone fail an evaluation
//
//	Given round trips that open and close at the same price, so realised P&L
//	  is exactly zero
//	When accumulated commission passes the daily loss limit
//	Then the challenge fails on fees alone.
func TestCommissionsAloneFailTheChallenge(t *testing.T) {
	a := mustAccount(t, 5_000_000, 50)
	c := newChallenge(t)

	openAccount(t, c, 1, "day-1", a, 20_000)

	// 2,000 contracts at 50 cents is exactly the $1,000 limit.
	mustFill(t, a, market.SideBuy, 1_000, 20_000)
	mustFill(t, a, market.SideSell, 1_000, 20_000)
	if a.FeesCts() != 100_000 {
		t.Fatalf("fees: got %d, want 100000", a.FeesCts())
	}

	observeAccount(t, c, 2, "day-1", a, 20_000)
	if c.State() != challenge.StateActive {
		t.Fatalf("state: got %v, want active at exactly the limit", c.State())
	}

	mustFill(t, a, market.SideBuy, 1, 20_000)
	mustFill(t, a, market.SideSell, 1, 20_000)

	observeAccount(t, c, 3, "day-1", a, 20_000)
	if c.State() != challenge.StateFailed {
		t.Fatalf("state: got %v, want failed", c.State())
	}

	if a.RealisedCts() != 0 {
		t.Fatalf("realised: got %d, want 0 — every round trip closed at its entry price", a.RealisedCts())
	}
	if p, _ := a.Position(mnq); !p.IsFlat() {
		t.Fatalf("position: got %+v, want flat", p)
	}
}

// Scenario: a position held across a boundary carries its open loss into the
// new session's reference
//
//	Given a long that is down $500 when a session ends
//	When the next session opens at the account's marked-to-market equity
//	Then the new reference already contains that open loss, and only a
//	  further $1,000 fails the challenge.
func TestTheNewSessionReferenceIsMarkedToMarket(t *testing.T) {
	a := mustAccount(t, 5_000_000, 0)
	c := newChallenge(t)

	openAccount(t, c, 1, "day-1", a, 20_000)
	mustFill(t, a, market.SideBuy, 10, 20_000)

	// Down $500 at the close of day one, well inside the limit.
	observeAccount(t, c, 2, "day-1", a, 19_900)
	if c.State() != challenge.StateActive {
		t.Fatalf("state: got %v, want active", c.State())
	}

	balance, reference := valuationAt(t, a, 19_900)
	if reference != 4_950_000 {
		t.Fatalf("reference: got %d, want 4950000 — the open loss must be in it", reference)
	}
	if balance != 5_000_000 {
		t.Fatalf("balance: got %d, want 5000000 — nothing has been realised", balance)
	}
	mustOpen(t, c, openedAt(3, "day-2", balance, reference))

	// The position is untouched by the boundary.
	if p, _ := a.Position(mnq); p.NetQty != 10 || p.CostBasisCts != 10_000_000 {
		t.Fatalf("the boundary changed the position: %+v", p)
	}

	// Another $1,000 down from the new reference, exactly at the limit.
	observeAccount(t, c, 4, "day-2", a, 19_700)
	if c.State() != challenge.StateActive {
		t.Fatalf("state: got %v, want active at exactly the limit", c.State())
	}

	observeAccount(t, c, 5, "day-2", a, 19_699)
	if c.State() != challenge.StateFailed {
		t.Fatalf("state: got %v, want failed", c.State())
	}
}

// Scenario: an unrealised gain does not pass an evaluation, and realising it
// does
//
//	Given an open long whose unrealised gain reaches the target level
//	When it is observed while still open
//	Then the challenge is still active, because a position that touches the
//	  target for an instant and gives it back must not have bought an
//	  irreversible approval
//	And when the position is closed at that same price, the gain becomes
//	  balance and the challenge passes.
func TestAnUnrealisedGainDoesNotPassUntilItIsRealised(t *testing.T) {
	a := mustAccount(t, 5_000_000, 0)
	c := withTarget(t)

	openAccount(t, c, 1, "day-1", a, 20_000)
	mustFill(t, a, market.SideBuy, 10, 20_000)

	// $1,000 up on the mark: the target level, on equity alone.
	observeAccount(t, c, 2, "day-1", a, 20_200)
	if c.State() != challenge.StateActive {
		t.Fatalf("state: got %v, want active while the gain is unrealised", c.State())
	}
	if a.RealisedCts() != 0 {
		t.Fatalf("realised: got %d, want 0", a.RealisedCts())
	}
	if p, _ := a.Position(mnq); p.NetQty != 10 {
		t.Fatalf("position: got %d, want the long still open", p.NetQty)
	}

	// Giving it all back changes nothing, because nothing had been passed.
	observeAccount(t, c, 3, "day-1", a, 20_000)
	if c.State() != challenge.StateActive {
		t.Fatalf("state: got %v, want active after the gain was given back", c.State())
	}

	// Earning it again and closing turns it into balance.
	mustFill(t, a, market.SideSell, 10, 20_200)
	if a.RealisedCts() != 100_000 {
		t.Fatalf("realised: got %d, want 100000", a.RealisedCts())
	}

	observeAccount(t, c, 4, "day-1", a, 20_200)
	if c.State() != challenge.StatePassed {
		t.Fatalf("state: got %v, want passed once the gain is realised", c.State())
	}
}

// Scenario: commissions hold an evaluation back from its target
//
//	Given trades whose realised P&L is exactly the $1,000 target
//	When commission is charged on every contract
//	Then the evaluation has not passed, because it is measured on equity and
//	  equity is net of fees.
func TestCommissionsHoldTheChallengeBackFromTheTarget(t *testing.T) {
	a := mustAccount(t, 5_000_000, 50)
	c := withTarget(t)

	openAccount(t, c, 1, "day-1", a, 20_000)

	mustFill(t, a, market.SideBuy, 10, 20_000)
	mustFill(t, a, market.SideSell, 10, 20_200)

	if a.RealisedCts() != profitTargetCts {
		t.Fatalf("realised: got %d, want exactly the target %d", a.RealisedCts(), profitTargetCts)
	}
	if a.FeesCts() != 1_000 {
		t.Fatalf("fees: got %d, want 1000 for twenty contracts", a.FeesCts())
	}

	observeAccount(t, c, 2, "day-1", a, 20_200)
	if c.State() != challenge.StateActive {
		t.Fatalf("state: got %v, want active — fees leave it $10 short", c.State())
	}

	// Earn more than the fees took.
	mustFill(t, a, market.SideBuy, 1, 20_000)
	mustFill(t, a, market.SideSell, 1, 20_300)

	observeAccount(t, c, 3, "day-1", a, 20_300)
	if c.State() != challenge.StatePassed {
		t.Fatalf("state: got %v, want passed", c.State())
	}
}

func withAccountFloor(t *testing.T) *challenge.Challenge {
	t.Helper()
	c, err := challenge.New(challenge.Rules{
		StartingBalanceCts: 5_000_000,
		MaxDailyLossCts:    1_000_000, // large, so only the floor can fire
		MaxTotalLossCts:    200_000,   // floor at $48,000
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// Scenario: an unrealised loss crosses the static floor with nothing closed
//
//	Given a $50,000 evaluation with a floor at $48,000 and an open long
//	When the mark falls far enough that equity is below the floor
//	Then the challenge fails, although the balance has not moved at all —
//	  which is what proves the floor is measured on equity and not on
//	  settled money.
func TestUnrealisedLossCrossesTheStaticFloor(t *testing.T) {
	a := mustAccount(t, 5_000_000, 0)
	c := withAccountFloor(t)

	openAccount(t, c, 1, "d1", a, 20_000)
	mustFill(t, a, market.SideBuy, 10, 20_000)

	observeAccount(t, c, 2, "d1", a, 19_600)
	if c.State() != challenge.StateActive {
		t.Fatalf("state: got %v, want active exactly at the floor", c.State())
	}

	observeAccount(t, c, 3, "d1", a, 19_599)
	if c.State() != challenge.StateFailed || c.FailureReason() != challenge.FailureStaticDrawdown {
		t.Fatalf("got %v %v, want failed on static drawdown", c.State(), c.FailureReason())
	}

	balance, equity := valuationAt(t, a, 19_599)
	floor, ok := c.StaticFloor()
	if !ok {
		t.Fatal("static drawdown is not enabled")
	}
	if balance <= floor {
		t.Fatalf("balance %d is itself below the floor: the test proves nothing", balance)
	}
	if equity >= floor {
		t.Fatalf("equity %d is not below the floor", equity)
	}
	if a.RealisedCts() != 0 {
		t.Fatalf("realised: got %d, want 0 — nothing was closed", a.RealisedCts())
	}
}

// Scenario: commissions alone cross the static floor
//
//	Given round trips that open and close at the same price
//	When accumulated commission takes equity below the floor
//	Then the challenge fails on fees alone, with realised P&L still zero.
func TestCommissionsAloneCrossTheStaticFloor(t *testing.T) {
	a := mustAccount(t, 5_000_000, 50)
	c := withAccountFloor(t)

	openAccount(t, c, 1, "d1", a, 20_000)

	// 4,000 contracts at 50 cents is exactly the $2,000 floor.
	for i := 0; i < 2; i++ {
		mustFill(t, a, market.SideBuy, 1_000, 20_000)
		mustFill(t, a, market.SideSell, 1_000, 20_000)
	}
	if a.FeesCts() != 200_000 {
		t.Fatalf("fees: got %d, want 200000", a.FeesCts())
	}

	observeAccount(t, c, 2, "d1", a, 20_000)
	if c.State() != challenge.StateActive {
		t.Fatalf("state: got %v, want active exactly at the floor", c.State())
	}

	mustFill(t, a, market.SideBuy, 1, 20_000)
	mustFill(t, a, market.SideSell, 1, 20_000)

	observeAccount(t, c, 3, "d1", a, 20_000)
	if c.State() != challenge.StateFailed || c.FailureReason() != challenge.FailureStaticDrawdown {
		t.Fatalf("got %v %v, want failed on static drawdown", c.State(), c.FailureReason())
	}
	if a.RealisedCts() != 0 {
		t.Fatalf("realised: got %d, want 0 — every round trip closed at its entry price", a.RealisedCts())
	}
}

// Scenario: unrealised equity raises and then breaches the trailing floor
//
// Given an open long whose unrealised gain establishes a new high-water mark
// When the position gives back more than the configured trailing amount
// Then the challenge fails although balance and realised P&L never moved and
// the position remains open.
func TestUnrealisedEquityRaisesAndBreachesTheTrailingFloor(t *testing.T) {
	a := mustAccount(t, 5_000_000, 0)
	c, err := challenge.New(challenge.Rules{
		StartingBalanceCts:  5_000_000,
		MaxDailyLossCts:     1_000_000,
		TrailingDrawdownCts: 200_000,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	openAccount(t, c, 1, "d1", a, 20_000)
	mustFill(t, a, market.SideBuy, 10, 20_000)

	// 600 ticks * 10 contracts * 50 cents = $3,000 open gain.
	observeAccount(t, c, 2, "d1", a, 20_600)
	high, ok := c.HighWater()
	if !ok || high != 5_300_000 {
		t.Fatalf("high-water: got %d enabled %v, want 5300000 true", high, ok)
	}
	if threshold, ok := c.TrailingThresholdCts(); !ok || threshold != 5_100_000 {
		t.Fatalf("threshold: got %d enabled %v, want 5100000 true", threshold, ok)
	}

	// The open gain is now $995, below the $51,000 trailing floor.
	observeAccount(t, c, 3, "d1", a, 20_199)
	if c.State() != challenge.StateFailed || c.FailureReason() != challenge.FailureTrailingDrawdown {
		t.Fatalf("got %v %v, want failed on trailing drawdown", c.State(), c.FailureReason())
	}
	if a.RealisedCts() != 0 {
		t.Fatalf("realised: got %d, want 0", a.RealisedCts())
	}
	if balance, err := a.BalanceCts(); err != nil || balance != 5_000_000 {
		t.Fatalf("balance: got %d error %v, want 5000000", balance, err)
	}
	if p, _ := a.Position(mnq); p.NetQty != 10 {
		t.Fatalf("position: got %d, want the long still open", p.NetQty)
	}
}

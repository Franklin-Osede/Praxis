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

// equityAt is what an adapter would compute before stamping a snapshot.
func equityAt(t *testing.T, a *portfolio.Account, mark market.Ticks) market.Cents {
	t.Helper()
	marks := []portfolio.Mark{{Instrument: mnq, Price: mark}}
	equity, err := a.EquityCts(marks)
	if err != nil {
		t.Fatalf("EquityCts: %v", err)
	}
	return equity
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

	mustOpen(t, c, opened(1, "day-1", equityAt(t, a, 20_000)))
	mustFill(t, a, market.SideBuy, 10, 20_000)

	// Down exactly $1,000 on the mark: at the limit, not past it.
	mustObserve(t, c, snap(2, "day-1", equityAt(t, a, 19_800)))
	if c.State() != challenge.StateActive {
		t.Fatalf("state: got %v, want active at exactly the limit", c.State())
	}

	// One tick further is $5 more, comfortably past it.
	mustObserve(t, c, snap(3, "day-1", equityAt(t, a, 19_799)))
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

	mustOpen(t, c, opened(1, "day-1", equityAt(t, a, 20_000)))

	// 2,000 contracts at 50 cents is exactly the $1,000 limit.
	mustFill(t, a, market.SideBuy, 1_000, 20_000)
	mustFill(t, a, market.SideSell, 1_000, 20_000)
	if a.FeesCts() != 100_000 {
		t.Fatalf("fees: got %d, want 100000", a.FeesCts())
	}

	mustObserve(t, c, snap(2, "day-1", equityAt(t, a, 20_000)))
	if c.State() != challenge.StateActive {
		t.Fatalf("state: got %v, want active at exactly the limit", c.State())
	}

	mustFill(t, a, market.SideBuy, 1, 20_000)
	mustFill(t, a, market.SideSell, 1, 20_000)

	mustObserve(t, c, snap(3, "day-1", equityAt(t, a, 20_000)))
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

	mustOpen(t, c, opened(1, "day-1", equityAt(t, a, 20_000)))
	mustFill(t, a, market.SideBuy, 10, 20_000)

	// Down $500 at the close of day one, well inside the limit.
	mustObserve(t, c, snap(2, "day-1", equityAt(t, a, 19_900)))
	if c.State() != challenge.StateActive {
		t.Fatalf("state: got %v, want active", c.State())
	}

	reference := equityAt(t, a, 19_900)
	if reference != 4_950_000 {
		t.Fatalf("reference: got %d, want 4950000 — the open loss must be in it", reference)
	}
	mustOpen(t, c, opened(3, "day-2", reference))

	// The position is untouched by the boundary.
	if p, _ := a.Position(mnq); p.NetQty != 10 || p.CostBasisCts != 10_000_000 {
		t.Fatalf("the boundary changed the position: %+v", p)
	}

	// Another $1,000 down from the new reference, exactly at the limit.
	mustObserve(t, c, snap(4, "day-2", equityAt(t, a, 19_700)))
	if c.State() != challenge.StateActive {
		t.Fatalf("state: got %v, want active at exactly the limit", c.State())
	}

	mustObserve(t, c, snap(5, "day-2", equityAt(t, a, 19_699)))
	if c.State() != challenge.StateFailed {
		t.Fatalf("state: got %v, want failed", c.State())
	}
}

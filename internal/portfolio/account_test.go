package portfolio_test

import (
	"errors"
	"math"
	"math/rand"
	"reflect"
	"testing"

	"praxis/internal/market"
	"praxis/internal/portfolio"
)

// MNQ moves in 0.25 index points worth $0.50, so one tick is 50 cents.
var (
	mnq = market.Instrument{Symbol: "MNQ", CentsPerTick: 50}
	mes = market.Instrument{Symbol: "MES", CentsPerTick: 125}
)

const (
	startingCts   = 5_000_000 // $50,000
	commissionCts = 50        // $0.50 per contract
)

func fillOn(i market.Instrument, side market.Side, qty market.Qty, price market.Ticks) market.Fill {
	return market.Fill{OrderID: "o-1", Instrument: i, Time: 1, Side: side, Price: price, Qty: qty}
}

func fill(side market.Side, qty market.Qty, price market.Ticks) market.Fill {
	return fillOn(mnq, side, qty, price)
}

func newAccount(t *testing.T) *portfolio.Account {
	t.Helper()
	a, err := portfolio.NewAccount(startingCts, commissionCts)
	if err != nil {
		t.Fatalf("NewAccount: %v", err)
	}
	return a
}

func apply(t *testing.T, a *portfolio.Account, fills ...market.Fill) []portfolio.PositionEvent {
	t.Helper()
	var events []portfolio.PositionEvent
	for n, f := range fills {
		ev, err := a.ApplyFill(f)
		if err != nil {
			t.Fatalf("ApplyFill %d: %v", n, err)
		}
		events = append(events, ev...)
	}
	return events
}

func wantPosition(t *testing.T, a *portfolio.Account, netQty market.Qty, costBasisCts market.Cents) {
	t.Helper()
	p, ok := a.Position(mnq)
	if !ok {
		t.Fatal("no position in MNQ")
	}
	if p.NetQty != netQty || p.CostBasisCts != costBasisCts {
		t.Fatalf("position: got net %d basis %d, want net %d basis %d", p.NetQty, p.CostBasisCts, netQty, costBasisCts)
	}
}

// Scenario: a fill opens a position at its exact cost
//
//	Given a flat account
//	When a buy of 2 contracts fills at 20000 ticks
//	Then the position is long 2 at a cost basis of 2 * 20000 * 50 cents,
//	  a commission is charged per contract, and the open is recorded.
func TestOpenLong(t *testing.T) {
	a := newAccount(t)
	events := apply(t, a, fill(market.SideBuy, 2, 20_000))

	wantPosition(t, a, 2, 2_000_000)
	if a.RealisedCts() != 0 {
		t.Fatalf("realised: got %d, want 0", a.RealisedCts())
	}
	if a.FeesCts() != 100 {
		t.Fatalf("fees: got %d, want 100", a.FeesCts())
	}
	want := []portfolio.PositionEvent{{
		Kind: portfolio.PositionOpened, Instrument: mnq, Side: market.SideBuy,
		Qty: 2, Price: 20_000, FeeCts: 100,
	}}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events\n got: %+v\nwant: %+v", events, want)
	}
}

func TestOpenShort(t *testing.T) {
	a := newAccount(t)
	events := apply(t, a, fill(market.SideSell, 2, 20_000))

	wantPosition(t, a, -2, -2_000_000)
	if len(events) != 1 || events[0].Kind != portfolio.PositionOpened || events[0].Side != market.SideSell {
		t.Fatalf("events: got %+v, want one short open", events)
	}
}

// Scenario: adding to a position accumulates exact cost, never an average
//
//	Given a long of 2 at 20000
//	When 3 more fill at 20010
//	Then the cost basis is the exact sum of both fills, and the derived
//	  average price is display only.
func TestAddToLongAccumulatesExactCost(t *testing.T) {
	a := newAccount(t)
	events := apply(t, a,
		fill(market.SideBuy, 2, 20_000),
		fill(market.SideBuy, 3, 20_010),
	)

	wantPosition(t, a, 5, 5_001_500)
	if events[1].Kind != portfolio.PositionIncreased {
		t.Fatalf("second event: got %v, want increased", events[1].Kind)
	}

	p, _ := a.Position(mnq)
	if got := p.AvgPx(); got != 20_006 {
		t.Fatalf("AvgPx: got %d, want 20006", got)
	}
}

func TestAddToShortAccumulatesExactCost(t *testing.T) {
	a := newAccount(t)
	apply(t, a,
		fill(market.SideSell, 2, 20_000),
		fill(market.SideSell, 3, 19_990),
	)
	wantPosition(t, a, -5, -4_998_500)
}

// Scenario: a partial close realises P&L in proportion to the cost removed
//
//	Given a long of 5 with a cost basis of 5001500
//	When 2 are sold at 20020
//	Then two fifths of the cost basis is removed and the realised P&L is the
//	  proceeds minus exactly that.
func TestPartialCloseUsesWeightedAverageCost(t *testing.T) {
	a := newAccount(t)
	apply(t, a,
		fill(market.SideBuy, 2, 20_000),
		fill(market.SideBuy, 3, 20_010),
	)
	events := apply(t, a, fill(market.SideSell, 2, 20_020))

	wantPosition(t, a, 3, 3_000_900)
	if a.RealisedCts() != 1_400 {
		t.Fatalf("realised: got %d, want 1400", a.RealisedCts())
	}
	last := events[len(events)-1]
	if last.Kind != portfolio.PositionReduced || last.RealisedCts != 1_400 {
		t.Fatalf("event: got %+v, want reduced realising 1400", last)
	}
}

// Scenario: a position that closes to zero stays flat, it is not deleted
func TestFullCloseLeavesAFlatPosition(t *testing.T) {
	a := newAccount(t)
	apply(t, a,
		fill(market.SideBuy, 2, 20_000),
		fill(market.SideBuy, 3, 20_010),
		fill(market.SideSell, 2, 20_020),
	)
	events := apply(t, a, fill(market.SideSell, 3, 20_030))

	wantPosition(t, a, 0, 0)
	if a.RealisedCts() != 5_000 {
		t.Fatalf("realised: got %d, want 5000", a.RealisedCts())
	}
	if a.FeesCts() != 500 {
		t.Fatalf("fees: got %d, want 500 for ten contracts", a.FeesCts())
	}
	last := events[len(events)-1]
	if last.Kind != portfolio.PositionClosed {
		t.Fatalf("event: got %v, want closed", last.Kind)
	}
	if _, ok := a.Position(mnq); !ok {
		t.Fatal("the flat position was deleted")
	}
}

// Scenario: a flip is a close and a new open, charged on both legs
//
//	Given a long of 3 at 20000
//	When 5 are sold at 20010
//	Then 3 are closed and realised, a short of 2 opens with a clean cost
//	  basis, and commission is charged on all five contracts.
func TestLongToShortFlip(t *testing.T) {
	a := newAccount(t)
	apply(t, a, fill(market.SideBuy, 3, 20_000))
	events := apply(t, a, fill(market.SideSell, 5, 20_010))

	wantPosition(t, a, -2, -2_001_000)
	if a.RealisedCts() != 1_500 {
		t.Fatalf("realised: got %d, want 1500", a.RealisedCts())
	}

	want := []portfolio.PositionEvent{
		{Kind: portfolio.PositionClosed, Instrument: mnq, Side: market.SideSell, Qty: 3, Price: 20_010, RealisedCts: 1_500, FeeCts: 150},
		{Kind: portfolio.PositionOpened, Instrument: mnq, Side: market.SideSell, Qty: 2, Price: 20_010, FeeCts: 100},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("flip legs\n got: %+v\nwant: %+v", events, want)
	}
	if a.FeesCts() != 150+150+100 {
		t.Fatalf("fees: got %d, want 400", a.FeesCts())
	}
}

func TestShortToLongFlip(t *testing.T) {
	a := newAccount(t)
	apply(t, a, fill(market.SideSell, 3, 20_000))
	apply(t, a, fill(market.SideBuy, 5, 19_990))

	wantPosition(t, a, 2, 1_999_000)
	if a.RealisedCts() != 1_500 {
		t.Fatalf("realised: got %d, want 1500", a.RealisedCts())
	}
}

// Scenario: rounding an inexact cost allocation goes against the trader
//
//	Given a long of 3 whose cost basis is not divisible by three
//	When one contract is closed
//	Then the cost removed is rounded up, so the realised P&L is rounded down
//	  toward the loss, and the remainder stays in the residual basis.
func TestInexactCostAllocationRoundsAgainstTheTrader(t *testing.T) {
	a := newAccount(t)
	apply(t, a,
		fill(market.SideBuy, 2, 20_000),
		fill(market.SideBuy, 1, 20_001),
	)
	wantPosition(t, a, 3, 3_000_050)

	apply(t, a, fill(market.SideSell, 1, 20_010))

	// Exactly one third of 3000050 is 1000016.67; the trader is charged 1000017.
	wantPosition(t, a, 2, 2_000_033)
	if a.RealisedCts() != 483 {
		t.Fatalf("realised: got %d, want 483 (the exact figure is 483.33)", a.RealisedCts())
	}
}

// Scenario: a short's cost allocation rounds against the trader too
//
// A short's basis is negative, and Go truncates a negative quotient toward
// zero — which removes less of the basis than the exact share, and therefore
// realises less. That is the direction the rule requires, and it is a
// different code path from the long case: the existing long test passes under
// an implementation that rounds a short the wrong way.
func TestAShortsCostAllocationAlsoRoundsAgainstTheTrader(t *testing.T) {
	a := newAccount(t)
	apply(t, a,
		fill(market.SideSell, 2, 100),
		fill(market.SideSell, 1, 101),
	)
	// Two contracts at 100 and one at 101, each tick worth 50 cents.
	wantPosition(t, a, -3, -15_050)

	apply(t, a, fill(market.SideBuy, 1, 100))

	// One third of -15050 is -5016.67. Removing -5017 would leave the trader
	// with 17 cents; removing -5016 leaves 16, and 16 is the honest figure.
	wantPosition(t, a, -2, -10_034)
	if a.RealisedCts() != 16 {
		t.Fatalf("realised: got %d, want 16 (the exact figure is 16.67 in the trader's favour)", a.RealisedCts())
	}
}

// Property: closing a position in pieces removes exactly the basis it had
//
// Whatever order and however many pieces, the shares taken out of the cost
// basis add up to the basis that was there. A rounding rule that leaked in
// either direction would show up here as a residue, and a rule that leaked
// consistently in the trader's favour is the failure this project cares most
// about.
func TestPropertyPartialClosesRemoveExactlyTheBasis(t *testing.T) {
	random := rand.New(rand.NewSource(20260905))

	for run := 0; run < 400; run++ {
		long := random.Intn(2) == 0
		entry, exit := market.SideBuy, market.SideSell
		if !long {
			entry, exit = market.SideSell, market.SideBuy
		}

		a := newAccount(t)
		var opened market.Qty
		for legs := 1 + random.Intn(3); legs > 0; legs-- {
			qty := market.Qty(1 + random.Intn(4))
			apply(t, a, fill(entry, qty, market.Ticks(20_000+random.Intn(20))))
			opened += qty
		}
		position, _ := a.Position(mnq)
		basis := position.CostBasisCts

		var removed market.Cents
		for opened > 0 {
			qty := market.Qty(1 + random.Intn(int(opened)))
			before, _ := a.Position(mnq)
			apply(t, a, fill(exit, qty, market.Ticks(20_000+random.Intn(20))))
			after, _ := a.Position(mnq)
			removed += before.CostBasisCts - after.CostBasisCts
			opened -= qty
		}

		if removed != basis {
			t.Fatalf("run %d: closing removed %d of a basis of %d", run, removed, basis)
		}
		if position, _ := a.Position(mnq); position.CostBasisCts != 0 {
			t.Fatalf("run %d: a flat position holds a basis of %d", run, position.CostBasisCts)
		}
	}
}

// Scenario: closing in pieces realises exactly what closing at once would
func TestPartialClosesConserveRealisedPnL(t *testing.T) {
	pieces := newAccount(t)
	apply(t, pieces,
		fill(market.SideBuy, 2, 20_000),
		fill(market.SideBuy, 1, 20_001),
		fill(market.SideSell, 1, 20_010),
		fill(market.SideSell, 2, 20_010),
	)

	atOnce := newAccount(t)
	apply(t, atOnce,
		fill(market.SideBuy, 2, 20_000),
		fill(market.SideBuy, 1, 20_001),
		fill(market.SideSell, 3, 20_010),
	)

	if pieces.RealisedCts() != atOnce.RealisedCts() {
		t.Fatalf("realised: %d in pieces, %d at once", pieces.RealisedCts(), atOnce.RealisedCts())
	}
}

// Scenario: equity is the starting balance plus realised, minus fees, plus
// the unrealised value of what is still open
func TestEquity(t *testing.T) {
	a := newAccount(t)
	apply(t, a,
		fill(market.SideBuy, 5, 20_000),
		fill(market.SideSell, 2, 20_020),
	)

	marks := []portfolio.Mark{{Instrument: mnq, Price: 20_030}}

	unrealised, err := a.UnrealisedCts(marks)
	if err != nil {
		t.Fatalf("UnrealisedCts: %v", err)
	}
	if unrealised != 4_500 {
		t.Fatalf("unrealised: got %d, want 4500", unrealised)
	}

	equity, err := a.EquityCts(marks)
	if err != nil {
		t.Fatalf("EquityCts: %v", err)
	}
	want := market.Cents(startingCts) + a.RealisedCts() - a.FeesCts() + unrealised
	if equity != want {
		t.Fatalf("equity: got %d, want %d", equity, want)
	}
	if equity != 5_000_000+2_000-350+4_500 {
		t.Fatalf("equity: got %d, want 5006150", equity)
	}
}

// Scenario: unrealised P&L is measured against the exact basis, never the
//
//	average price
//
// AvgPx is a rounded display figure and the position knows it — the comment on
// it says so. This is the case where the difference shows: three contracts
// whose basis is 15050 have an average of 100.33, which truncates to 100, and
// a valuation taken against 100 would report the position as flat when it is
// fifty cents down. UnrealisedCts feeds equity, and equity is what every loss
// rule reads, so the error would be a drawdown the trader never had to survive.
func TestUnrealisedIsMeasuredAgainstTheExactBasis(t *testing.T) {
	a := newAccount(t)
	apply(t, a,
		fill(market.SideBuy, 2, 100),
		fill(market.SideBuy, 1, 101),
	)
	wantPosition(t, a, 3, 15_050)

	position, _ := a.Position(mnq)
	if position.AvgPx() != 100 {
		t.Fatalf("the fixture does not truncate: AvgPx is %d, want 100", position.AvgPx())
	}

	got, err := a.UnrealisedCts([]portfolio.Mark{{Instrument: mnq, Price: 100}})
	if err != nil {
		t.Fatalf("UnrealisedCts: %v", err)
	}
	if got != -50 {
		t.Fatalf("unrealised: got %d, want -50 — the average price says 0, and the average price is rounded", got)
	}
}

func TestUnrealisedOfAShortRisesAsThePriceFalls(t *testing.T) {
	a := newAccount(t)
	apply(t, a, fill(market.SideSell, 2, 20_000))

	got, err := a.UnrealisedCts([]portfolio.Mark{{Instrument: mnq, Price: 19_990}})
	if err != nil {
		t.Fatalf("UnrealisedCts: %v", err)
	}
	if got != 1_000 {
		t.Fatalf("unrealised: got %d, want 1000", got)
	}
}

func TestEquityRequiresAMarkForEveryOpenPosition(t *testing.T) {
	a := newAccount(t)
	apply(t, a, fill(market.SideBuy, 1, 20_000))

	if _, err := a.EquityCts(nil); !errors.Is(err, portfolio.ErrMissingMark) {
		t.Fatalf("error: got %v, want %v", err, portfolio.ErrMissingMark)
	}
}

func TestFlatPositionsNeedNoMark(t *testing.T) {
	a := newAccount(t)
	apply(t, a,
		fill(market.SideBuy, 1, 20_000),
		fill(market.SideSell, 1, 20_010),
	)
	equity, err := a.EquityCts(nil)
	if err != nil {
		t.Fatalf("EquityCts: %v", err)
	}
	if equity != 5_000_000+500-100 {
		t.Fatalf("equity: got %d, want 5000400", equity)
	}
}

// Positions are held in instrument order, never in the order fills arrived,
// so nothing downstream can depend on application order.
func TestPositionsAreInInstrumentOrder(t *testing.T) {
	a := newAccount(t)
	apply(t, a,
		fillOn(mnq, market.SideBuy, 1, 20_000),
		fillOn(mes, market.SideBuy, 1, 5_000),
	)

	got := a.Positions()
	if len(got) != 2 || got[0].Instrument != mes || got[1].Instrument != mnq {
		t.Fatalf("positions: got %+v, want MES before MNQ", got)
	}
}

func TestAccountRejectsImpossibleInput(t *testing.T) {
	if _, err := portfolio.NewAccount(0, commissionCts); !errors.Is(err, portfolio.ErrNonPositiveBalance) {
		t.Fatalf("error: got %v, want %v", err, portfolio.ErrNonPositiveBalance)
	}
	if _, err := portfolio.NewAccount(startingCts, -1); !errors.Is(err, portfolio.ErrNegativeCommission) {
		t.Fatalf("error: got %v, want %v", err, portfolio.ErrNegativeCommission)
	}

	a, err := portfolio.NewAccount(startingCts, commissionCts)
	if err != nil {
		t.Fatalf("NewAccount: %v", err)
	}
	tests := []struct {
		name string
		fill market.Fill
		want error
	}{
		{"zero value fill", market.Fill{}, market.ErrEmptyOrderID},
		{"instrument without a tick value", fillOn(market.Instrument{Symbol: "MNQ"}, market.SideBuy, 1, 1), market.ErrNonPositiveTickValue},
		{"unspecified side", market.Fill{OrderID: "o-1", Instrument: mnq, Qty: 1, Price: 1}, market.ErrInvalidSide},
		{"non-positive quantity", market.Fill{OrderID: "o-1", Instrument: mnq, Side: market.SideBuy, Qty: 0, Price: 1}, market.ErrNonPositiveQty},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := a.ApplyFill(tc.fill)
			if !errors.Is(err, portfolio.ErrInvalidFill) {
				t.Fatalf("error class: got %v, want %v", err, portfolio.ErrInvalidFill)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("error cause: got %v, want %v", err, tc.want)
			}
			if got != nil {
				t.Fatalf("rejected fill produced events: %+v", got)
			}
		})
	}
}

// knownFills builds a fixed sequence of fills from an explicitly seeded
// source, ending flat so that the whole sequence can be checked against pure
// cash accounting.
func knownFills(r *rand.Rand) []market.Fill {
	fills := make([]market.Fill, 0, 100)
	var net market.Qty
	for i := 0; i < 99; i++ {
		side := market.SideBuy
		if r.Int63n(2) == 1 {
			side = market.SideSell
		}
		qty := market.Qty(r.Int63n(4) + 1)
		price := market.Ticks(r.Int63n(200) + 19_900)
		f := fill(side, qty, price)
		net += f.SignedQty()
		fills = append(fills, f)
	}
	if net > 0 {
		fills = append(fills, fill(market.SideSell, net, 20_000))
	} else if net < 0 {
		fills = append(fills, fill(market.SideBuy, -net, 20_000))
	} else {
		fills = append(fills, fill(market.SideBuy, 1, 20_000), fill(market.SideSell, 1, 20_000))
	}
	return fills
}

// Property: a known sequence of fills produces the same account every time,
// and its realised P&L matches an independent calculation that never touches
// cost basis. A sequence that ends flat has realised exactly the negative of
// its total signed cash flow; any cent lost to rounding would show up here.
func TestPropertyKnownFillsProduceAnIdenticalAndIndependentlyCheckedAccount(t *testing.T) {
	const runs = 20

	type snapshot struct {
		realised, fees market.Cents
		positions      []portfolio.Position
		events         []portfolio.PositionEvent
	}

	var baseline snapshot
	for run := 0; run < runs; run++ {
		r := rand.New(rand.NewSource(20240825))
		fills := knownFills(r)
		a := newAccount(t)
		var events []portfolio.PositionEvent
		for n, f := range fills {
			ev, err := a.ApplyFill(f)
			if err != nil {
				t.Fatalf("run %d fill %d: %v", run, n, err)
			}
			events = append(events, ev...)
		}

		got := snapshot{realised: a.RealisedCts(), fees: a.FeesCts(), positions: a.Positions(), events: events}
		if run == 0 {
			baseline = got

			var cashFlow, contracts market.Cents
			for _, f := range fills {
				money, err := f.Instrument.Money(f.Price, f.SignedQty())
				if err != nil {
					t.Fatalf("independent calculation overflowed: %v", err)
				}
				cashFlow += money
				contracts += market.Cents(f.Qty)
			}
			if got.realised != -cashFlow {
				t.Fatalf("realised %d does not match the independent cash calculation %d", got.realised, -cashFlow)
			}
			if got.fees != contracts*commissionCts {
				t.Fatalf("fees %d do not match %d contracts at %d", got.fees, contracts, commissionCts)
			}
			p, _ := a.Position(mnq)
			if !p.IsFlat() || p.CostBasisCts != 0 {
				t.Fatalf("the sequence did not end flat: %+v", p)
			}
			continue
		}
		if !reflect.DeepEqual(got, baseline) {
			t.Fatalf("run %d diverged from the baseline run", run)
		}
	}
}

// mnqWrong carries the MNQ symbol with a different monetary specification.
// Accepting it would let two incompatible cost bases accumulate in one
// position and manufacture a loss with no price movement at all.
var mnqWrong = market.Instrument{Symbol: "MNQ", CentsPerTick: 100}

type accountState struct {
	realised, fees market.Cents
	positions      []portfolio.Position
}

func snapshotOf(a *portfolio.Account) accountState {
	return accountState{realised: a.RealisedCts(), fees: a.FeesCts(), positions: a.Positions()}
}

func assertUnchanged(t *testing.T, a *portfolio.Account, before accountState) {
	t.Helper()
	if got := snapshotOf(a); !reflect.DeepEqual(got, before) {
		t.Fatalf("a rejected operation mutated the account\n got: %+v\nwas: %+v", got, before)
	}
}

// Scenario: one symbol has one monetary specification, for the life of the
// account
//
//	Given an account holding MNQ at 50 cents per tick
//	When a fill arrives labelled MNQ at 100 cents per tick
//	Then it is rejected and nothing about the account changes.
func TestRejectsTheSameSymbolWithADifferentTickValue(t *testing.T) {
	a := newAccount(t)
	apply(t, a, fill(market.SideBuy, 1, 20_000))
	before := snapshotOf(a)

	got, err := a.ApplyFill(fillOn(mnqWrong, market.SideBuy, 1, 20_000))
	if !errors.Is(err, portfolio.ErrInstrumentSpecMismatch) {
		t.Fatalf("error: got %v, want %v", err, portfolio.ErrInstrumentSpecMismatch)
	}
	if got != nil {
		t.Fatalf("rejected fill produced events: %+v", got)
	}
	assertUnchanged(t, a, before)

	if len(a.Positions()) != 1 {
		t.Fatalf("positions: got %d, want the rejected fill to create none", len(a.Positions()))
	}
}

func TestRejectsAMarkWithADifferentTickValue(t *testing.T) {
	a := newAccount(t)
	apply(t, a, fill(market.SideBuy, 1, 20_000))

	if _, err := a.UnrealisedCts([]portfolio.Mark{{Instrument: mnqWrong, Price: 20_000}}); !errors.Is(err, portfolio.ErrInstrumentSpecMismatch) {
		t.Fatalf("error: got %v, want %v", err, portfolio.ErrInstrumentSpecMismatch)
	}
	if _, err := a.EquityCts([]portfolio.Mark{{Instrument: mnqWrong, Price: 20_000}}); !errors.Is(err, portfolio.ErrInstrumentSpecMismatch) {
		t.Fatalf("error: got %v, want %v", err, portfolio.ErrInstrumentSpecMismatch)
	}
}

func TestRejectsANonPositiveMarkPrice(t *testing.T) {
	a := newAccount(t)
	apply(t, a, fill(market.SideBuy, 1, 20_000))

	if _, err := a.UnrealisedCts([]portfolio.Mark{{Instrument: mnq, Price: 0}}); !errors.Is(err, market.ErrNonPositivePrice) {
		t.Fatalf("error: got %v, want %v", err, market.ErrNonPositivePrice)
	}
}

func TestRejectsANonPositiveFillPrice(t *testing.T) {
	a := newAccount(t)
	before := snapshotOf(a)

	_, err := a.ApplyFill(market.Fill{OrderID: "o-1", Instrument: mnq, Side: market.SideBuy, Qty: 1, Price: 0})
	if !errors.Is(err, market.ErrNonPositivePrice) {
		t.Fatalf("error: got %v, want %v", err, market.ErrNonPositivePrice)
	}
	assertUnchanged(t, a, before)
}

// Scenario: arithmetic that cannot be represented is an error, not a wrap
//
//	Given an instrument whose tick value is near the integer limit
//	When a fill would overflow the conversion from ticks to money
//	Then the fill is rejected and the account is untouched.
func TestOverflowInTickToMoneyLeavesTheAccountUnchanged(t *testing.T) {
	huge := market.Instrument{Symbol: "HUGE", CentsPerTick: math.MaxInt64 / 2}
	a := newAccount(t)
	before := snapshotOf(a)

	got, err := a.ApplyFill(fillOn(huge, market.SideBuy, 1, 3))
	if !errors.Is(err, market.ErrOverflow) {
		t.Fatalf("error: got %v, want %v", err, market.ErrOverflow)
	}
	if got != nil {
		t.Fatalf("overflowing fill produced events: %+v", got)
	}
	assertUnchanged(t, a, before)
}

func TestOverflowWhileAddingToAPositionLeavesTheAccountUnchanged(t *testing.T) {
	unit := market.Instrument{Symbol: "UNIT", CentsPerTick: 1}
	// No commission, so the overflow under test is the position's own cost.
	a, err := portfolio.NewAccount(startingCts, 0)
	if err != nil {
		t.Fatalf("NewAccount: %v", err)
	}
	apply(t, a, fillOn(unit, market.SideBuy, math.MaxInt64/2, 1))
	before := snapshotOf(a)

	got, err := a.ApplyFill(fillOn(unit, market.SideBuy, math.MaxInt64/2+2, 1))
	if !errors.Is(err, market.ErrOverflow) {
		t.Fatalf("error: got %v, want %v", err, market.ErrOverflow)
	}
	if got != nil {
		t.Fatalf("overflowing fill produced events: %+v", got)
	}
	assertUnchanged(t, a, before)
}

func TestOverflowInEquityIsReported(t *testing.T) {
	unit := market.Instrument{Symbol: "UNIT", CentsPerTick: 1}
	a, err := portfolio.NewAccount(math.MaxInt64-10, 0)
	if err != nil {
		t.Fatalf("NewAccount: %v", err)
	}
	if _, err := a.ApplyFill(fillOn(unit, market.SideBuy, 1_000, 1_000)); err != nil {
		t.Fatalf("ApplyFill: %v", err)
	}

	if _, err := a.EquityCts([]portfolio.Mark{{Instrument: unit, Price: math.MaxInt64 / 1_000}}); !errors.Is(err, market.ErrOverflow) {
		t.Fatalf("error: got %v, want %v", err, market.ErrOverflow)
	}
}

// Scenario: a position is flat exactly when it holds no cost
func TestPositionValidateEnforcesFlatnessAndSign(t *testing.T) {
	tests := []struct {
		name string
		pos  portfolio.Position
		want error
	}{
		{"flat and empty", portfolio.Position{Instrument: mnq}, nil},
		{"long with a positive basis", portfolio.Position{Instrument: mnq, NetQty: 1, CostBasisCts: 1_000_000}, nil},
		{"short with a negative basis", portfolio.Position{Instrument: mnq, NetQty: -1, CostBasisCts: -1_000_000}, nil},
		{"flat holding a cost basis", portfolio.Position{Instrument: mnq, CostBasisCts: 1}, portfolio.ErrFlatWithCostBasis},
		{"open holding no cost basis", portfolio.Position{Instrument: mnq, NetQty: 1}, portfolio.ErrOpenWithoutCostBasis},
		{"long with a negative basis", portfolio.Position{Instrument: mnq, NetQty: 1, CostBasisCts: -1}, portfolio.ErrCostBasisSign},
		{"short with a positive basis", portfolio.Position{Instrument: mnq, NetQty: -1, CostBasisCts: 1}, portfolio.ErrCostBasisSign},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.pos.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("error: got %v, want %v", err, tc.want)
			}
		})
	}
}

// Property: every position an account holds is internally coherent after
// every fill, whatever the sequence.
func TestPropertyAccountPositionsStayCoherent(t *testing.T) {
	r := rand.New(rand.NewSource(20240826))
	a := newAccount(t)

	for i := 0; i < 2_000; i++ {
		side := market.SideBuy
		if r.Int63n(2) == 1 {
			side = market.SideSell
		}
		f := fill(side, market.Qty(r.Int63n(4)+1), market.Ticks(r.Int63n(200)+19_900))
		if _, err := a.ApplyFill(f); err != nil {
			t.Fatalf("iteration %d: legal fill rejected: %v", i, err)
		}
		for _, p := range a.Positions() {
			if err := p.Validate(); err != nil {
				t.Fatalf("iteration %d: incoherent position %+v: %v", i, p, err)
			}
		}
	}
}

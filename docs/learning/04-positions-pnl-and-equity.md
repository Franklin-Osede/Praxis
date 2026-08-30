# 4 — Positions, P&L and equity

This is where a trading simulator becomes an accounting machine.

## Position

A position is what you currently hold in one instrument.

```go
type Position struct {
    Instrument   market.Instrument
    NetQty       market.Qty    // signed: + long, - short, 0 flat
    CostBasisCts market.Cents  // exact signed cash committed
}
```

Two things are deliberate and easy to get wrong.

**`NetQty` is signed here** and nowhere else. Orders and fills use a positive
quantity plus a `Side`. A position is a net holding, so long 3 is `+3` and short
2 is `-2`.

**`CostBasisCts` is exact cash, not an average price.** A rounded average entry
price loses precision on every partial fill, and those losses compound. Praxis
stores what you actually committed:

```
Buy 2 at 20,000 ticks, 50 cents per tick
  cost = 2 * 20,000 * 50 = 2,000,000 cents
```

`AvgPx()` exists for display and is forbidden in any P&L or risk calculation.

## Adding to a position

Costs accumulate exactly. Nothing is averaged.

```
Buy 2 at 20,000  ->  basis 2,000,000   net 2
Buy 3 at 20,010  ->  basis 5,001,500   net 5
```

## Unrealised P&L

What the position would make if closed right now:

```
unrealised = value at mark - cost basis
           = Money(mark, netQty) - costBasis
```

The sign convention makes shorts work with no special case:

```
Short 2 at 20,000  ->  basis -2,000,000
Mark at 19,990     ->  value -1,999,000
unrealised = -1,999,000 - (-2,000,000) = +1,000
```

A short gains as the price falls, and the arithmetic says so without a branch.

## Closing part of a position

Praxis uses **weighted average cost**, not FIFO lot tracking. Closing two of
five removes two fifths of the cost:

```
Long 5, basis 5,001,500. Sell 2 at 20,020.

removed   = 5,001,500 * 2 / 5 = 2,000,600
proceeds  = 2 * 20,020 * 50   = 2,002,000
realised  = 2,002,000 - 2,000,600 = 1,400 cents

remaining: net 3, basis 3,000,900
```

### When the division is not exact

Two rules keep this honest.

**The allocated cost rounds up** — toward positive infinity. That reduces
realised P&L for a long and for a short alike, so an inexact split always costs
the trader rather than quietly paying them.

**The residual basis is what is left after subtracting**, never a number
computed separately. That means the allocations over any sequence of partial
closes add up to exactly the original basis. Closing in three pieces realises
exactly what closing at once would. No cent is created or destroyed.

## Flipping

Selling 5 when you are long 3 is not one action. It is two:

```
Long 3 at 20,000, sell 5 at 20,010

1. close 3   -> realise 1,500 cents, commission on 3
2. open short 2 at 20,010, clean basis, commission on 2
```

Praxis reports both legs as separate facts. The new short does not inherit
anything from the closed long.

## A closed position stays

A position that reaches zero keeps its instrument and emits `PositionClosed`.
It does not vanish. The account can still tell you it traded MNQ today.

## Equity

```
equity = starting balance + realised - fees + unrealised
```

That single line is what a challenge rule will be measured against, which is
why the account is defended so heavily:

- One symbol means one monetary specification. MNQ at 50 cents per tick and
  "MNQ" at 100 cents per tick are not the same instrument, and mixing them
  manufactures a loss with no price movement at all.
- Every operation uses checked arithmetic. Go's integer overflow is silent, and
  exact accounting is only exact until it happens.
- A rejected fill changes nothing: the whole new state is computed first and
  written only if all of it succeeded.
- A position is flat exactly when it holds no cost basis.

## Why integers, not floats

`0.1 + 0.2` is not `0.3` in binary floating point. Two runs of the same session
could disagree in the last cent, which makes them incomparable — and comparing
runs is the entire point of a deterministic simulator. Money is `Cents`, prices
are `Ticks`, and both are whole numbers.

## Common mistakes

- **"Average price times quantity is my cost."** Only if the average is exact,
  which after a few partial fills it is not.
- **"Unrealised P&L isn't real."** It is real enough to fail an evaluation.
- **"Commission is small."** Ten contracts at 50 cents is 500 cents, and it
  counts against a daily limit.

## What Praxis simplifies

Commission is a flat figure per contract on the account: no tiered schedule, no
exchange and clearing fees broken out, no per-instrument rate. There is no
margin, no interest and no currency conversion.

Code: [`internal/portfolio/`](../../internal/portfolio/).

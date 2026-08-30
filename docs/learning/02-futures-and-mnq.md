# 2 — What a futures contract is, and why MNQ

## The idea

A future is a standardised agreement whose value tracks something else. You are
not buying the thing; you are taking one side of a contract about it.

## The analogy

A farmer fears the price of oranges will fall before harvest. A supermarket
fears it will rise. Today they agree a reference price of 1,000 for delivery in
three months.

The farmer is now protected against a fall, the supermarket against a rise.
Neither has moved a single orange. That is what futures were invented for:
moving risk from someone who does not want it to someone who does.

## The financial version

MNQ — the Micro E-mini Nasdaq-100 — does the same thing with an index instead
of fruit. No crate of a hundred companies changes hands. The contract simply
gains or loses value as the index moves.

```
Buy MNQ with the index at 20,000
Close with the index at 20,010
Move: +10 index points
```

MNQ is worth **$2 per index point**, so that move is worth $20 per contract.
Ten points the other way is −$20.

Buying is called being **long**; selling is being **short**. You never have to
find your counterparty: the exchange matches orders and a clearing house stands
between the two sides, so neither depends on the other's solvency.

You exit by doing the opposite trade. Buy one, later sell one, and your position
is back to zero. You have opened and closed an obligation, not bought and sold a
company.

## Ticks

Prices do not move continuously. MNQ moves in steps of 0.25 index points:

```
1 tick   = 0.25 index points
1 point  = 4 ticks
1 tick   = $0.50 = 50 cents
```

That last line is the whole reason Praxis can hold money in integers:

```go
mnq := market.Instrument{Symbol: "MNQ", CentsPerTick: 50}
```

An instrument whose tick is not a whole number of cents is rejected rather than
rounded. See [4 — Positions, P&L and equity](04-positions-pnl-and-equity.md).

## Margin, and why futures are dangerous

You do not deposit the full economic value the contract represents. The broker
requires a deposit called **margin**, which is much smaller. The result is
**leverage**: a small amount of your money controls a much larger exposure.

A modest move in the index can be a large move relative to what you deposited.
MNQ is ten times smaller than the full-size NQ ($2 per point against $20), but
"micro" describes the size of the contract, not the size of the risk.

## Expiry

Futures expire. As the date approaches traders either close the position or
**roll** it into the next contract. Praxis models neither yet: a correct kernel
for one session and one contract comes first.

## Why MNQ was chosen

Not because it is a good way to make money — Praxis makes no such claim. The
reasons are scope and learning:

- **Small.** Ten times less money per point than NQ, with the same mechanics.
- **Clean arithmetic.** 0.25 points, $0.50, exactly 50 cents per tick.
- **Active.** Enough price changes to exercise orders, stops and gaps.
- **Relevant.** It is a common instrument in prop firm evaluations, which is
  what Praxis simulates.
- **One instrument removes ten problems.** Different tick values, sessions,
  expiries, time zones, liquidity and data sources can all wait.

The architecture allows more instruments. The product uses one. See ADR-009.

## Common mistakes

- **"I own part of the Nasdaq."** You hold a contract position, not shares.
- **"Micro means safe."** It means smaller, and leverage still applies.
- **"I can hold it forever."** Contracts expire; positions must be rolled or
  closed.

## What Praxis simplifies

No margin requirements, no margin calls, no expiry, no rollover, no overnight
funding. A position exists until you close it.

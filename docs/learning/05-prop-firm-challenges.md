# 5 — Prop firm evaluations, and what Praxis measures

## What an evaluation is

A proprietary trading firm offers to fund traders. Before it does, it runs an
**evaluation**: trade a simulated account under a fixed set of rules and reach a
profit target without breaking any of them.

The rules are the interesting part. They are not advice about how to trade —
they are hard constraints, and breaking one ends the evaluation immediately,
regardless of how profitable you were a minute earlier.

## The usual rules

**Profit target.** Reach a stated gain to pass.

**Maximum daily loss.** Lose more than X in a single trading day and the
evaluation fails, even if the account is up overall for the week.

**Maximum drawdown.** A floor under the account. Two kinds:

- *Static*: a fixed level. Start at 50,000 with a 2,000 drawdown, and the floor
  is 48,000 forever.
- *Trailing*: the floor follows your high-water mark upward and **never moves
  back down**. Run the account to 52,000 and the floor rises to 50,000. Give the
  profit back and you fail at 50,000 — a level that started as break-even.

Trailing drawdown is where most evaluations actually end, and it is where the
hard engineering cases live: unrealised equity can breach it without a single
trade being closed.

**Contract limit.** A cap on position size.

**Minimum trading days.** Pass in one lucky session and it does not count.

**Consistency rule.** No single day may account for too large a share of the
profit.

## Why the rules are configuration, not code

Topstep, FTMO and Apex differ in their numbers, not in their nature. So the
execution engine knows nothing about any of them:

> Execution produces fills. The challenge engine consumes fills and applies
> `ChallengeRules` **values**. There is no `if provider == ...` anywhere.

A firm is a set of numbers. Adding one is filling in a struct.

## What a "day" is, and why it is not midnight

CME's equity index session opens at 17:00 US Central and runs to 16:00 the
following afternoon. A trading day and a calendar day are offset by seven
hours, plus daylight saving. Resetting a daily loss limit at midnight would
reset it in the middle of a live session.

Praxis refuses to put that calendar in the domain. The adapter stamps every
observation with a `SessionID`; a change of identifier is a boundary; the kernel
just compares identifiers. Time zones live in the adapter's configuration. See
[ADR-011](../adr/011-session-boundaries.md).

The reference equity for a session is taken at its first observation and
includes unrealised P&L and fees, because the limit is measured on equity and
equity contains both. Counting only realised P&L would let someone sit on an
unbounded open loss and never fail.

## What Praxis is actually for

This is the part that separates it from the rest of the category.

Most traders who fail an evaluation do not fail because their strategy is
wrong. They fail because they did not follow it: they widened a stop, added to
a loser, or traded four more times after a bad morning. That is a behavioural
problem, and behavioural problems are measurable.

So Praxis records **decisions**, not just outcomes: the order you submitted and
what your day looked like when you submitted it, the stop you moved and whether
you moved it further away, the position you added to while it was losing.

A backtester models a strategy. Praxis models a person.

## What Praxis will never do

Stated plainly because the industry it lives in is full of the opposite:

- It does not predict prices, with indicators, machine learning or anything else.
- It does not generate signals or recommend trades.
- It never presents a P&L as a promise of returns.
- Whether discipline trained in simulation transfers to real money under real
  pressure is **unproven**. Praxis assumes it might, and Phase 4 exists to test
  that assumption honestly, with hypotheses frozen before any data is collected.

If no stable behavioural patterns appear in that experiment, the honest
conclusion is that there is nothing for adaptive training to train, and Praxis
stays a personal simulator. That criterion is written down in advance
deliberately, because with a small sample and freedom to look afterwards you can
always find something.

## Where this is going

Praxis now implements daily loss, a realised-balance profit target, static
drawdown and equity-based trailing drawdown. The next slice connects scripted
market input, execution, account valuation and challenge through one ordered
in-memory event stream before richer rules are added.

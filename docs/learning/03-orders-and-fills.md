# 3 — Orders and fills

An **order** is an instruction. A **fill** is what actually happened. They are
different things, and confusing them is where simulators start lying.

## The three order types Praxis executes

### Market

"Buy now, whatever it costs." It crosses the spread: a buy pays the ask, a sell
takes the bid.

```
Quote:  bid 20,000 (4 available)   ask 20,001 (3 available)

Market buy 2  ->  fills 2 at 20,001
Market sell 2 ->  fills 2 at 20,000
```

### Limit

"Buy, but never above this price." The limit is the **worst** price you accept.

```
Buy limit at 20,005, ask is 20,001  ->  executable
Buy limit at 20,000, ask is 20,001  ->  waits
```

### Stop

"If the market reaches this level, get me out." A stop is a **trigger**, not a
price. Once touched, it becomes a market order and takes whatever is there.

```
Sell stop at 19,990, bid falls to 19,990  ->  triggers, fills at 19,990
Sell stop at 19,990, market gaps to 19,950 ->  triggers, fills at 19,950
```

That second line is the one that hurts, and it is real.

## The rule that governs all of it

> Praxis lies in favour of the market, never in favour of the trader.

An optimistic simulator teaches habits the real market will not honour, which
is worse than no simulator. Concretely:

- A buy never fills below the ask; a sell never above the bid.
- **A limit fills at its limit, never better.** In a real market a resting
  limit order *can* be filled better. Praxis refuses to grant it. This is a
  deliberate pessimistic policy, written down in `PRAXIS_SPEC.md` §4 so that
  nobody later "fixes" it as a bug.
- A stop never fills better than its level; a gap fills worse.
- A fill never exceeds the size the book displayed. Asking for 10 against a
  displayed size of 3 fills 3. The rest is yours to carry.
- When a bar reaches both your stop and your target and does not say in which
  order, **the stop wins**.

## Ambiguity, and when it isn't ambiguity

If you only have one-minute bars, a bar with a low below your stop and a high
above your target genuinely does not tell you which came first. Praxis resolves
it against you and records `Ambiguous: true` — a fact about the observation.

But if the bar *opened* beyond your stop, the order of events is known: the open
is the first price the bar showed. Praxis fills at the open, worse than your
level, and marks the bar **not** ambiguous. Recording ambiguity there would put
a false fact in the behavioural log.

## Where the Go types are

```go
type Order struct {
    ID         string
    Instrument Instrument
    Side       Side       // buy or sell
    Type       OrderType  // market, limit, stop
    Qty        Qty        // always positive
    LimitPrice Ticks      // only for limit orders
    StopPrice  Ticks      // only for stop orders
}

type Fill struct {
    OrderID    string
    Instrument Instrument
    Time       LogicalTime
    Side       Side
    Price      Ticks
    Qty        Qty
}
```

An order carries the price its own type needs and is forbidden from carrying
the other, so a price left behind after changing an order's type cannot be
ignored in silence.

Execution lives in
[`internal/execution/conservative.go`](../../internal/execution/conservative.go)
and bar resolution in
[`internal/execution/intrabar.go`](../../internal/execution/intrabar.go).

## Common mistakes

- **"My stop guarantees my loss."** It guarantees a trigger. In a gap you can
  lose far more than the level suggests.
- **"I got filled, so my order is done."** Not necessarily: a partial fill
  leaves a remainder.
- **"Direction is the sign of the quantity."** In orders and fills it is not.
  Quantity is always positive and `Side` carries direction. Only a *position*
  uses a signed quantity.

## What Praxis simplifies

No queue position, no partial fills across multiple price levels, no order
rejection by the exchange, no latency between sending and executing, and no
resting order book — an order is evaluated against one observation and the
caller carries whatever did not fill.

Test fixtures use round numbers like 20,000 ticks. Those are ticks, not index
points, and are not realistic MNQ levels; they are chosen to make the arithmetic
readable.

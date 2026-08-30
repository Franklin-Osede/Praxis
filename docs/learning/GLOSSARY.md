# Glossary

Terms as Praxis uses them. Where a term has a Go type, it is named.

**Ask** — the lowest price anyone is currently willing to sell at. You buy at
the ask.

**Bid** — the highest price anyone is currently willing to buy at. You sell at
the bid.

**Spread** — ask minus bid. The cost of trading immediately.

**Locked book** — bid equal to ask. Legal, and tradable.

**Crossed book** — bid above ask. Not a market state; in Praxis it is invalid
data and rejected.

**Bar** (`market.Bar`) — a completed interval summarised as open, high, low and
close over `[StartTime, EndTime)`. Supplied by an adapter; the kernel never
builds one (ADR-010).

**Quote** (`market.Quote`) — a top-of-book observation: bid, ask and the size
available at each.

**Tick** (`market.Ticks`) — the smallest price increment. For MNQ, 0.25 index
points, worth $0.50. Prices in Praxis are whole numbers of ticks.

**Cents** (`market.Cents`) — the only money unit in the domain. An instrument
whose tick value is not a whole number of cents is rejected, not rounded.

**Instrument** (`market.Instrument`) — a symbol plus its immutable
`CentsPerTick`. Within an account a symbol names exactly one specification.

**Order** (`market.Order`) — an instruction. Market, limit or stop.

**Market order** — take the best available price now. Crosses the spread.

**Limit order** — a price bound. The *worst* price you accept. In Praxis it
fills at its limit and never better, which is deliberately pessimistic.

**Stop order** — a trigger level. Once the market reaches it, it behaves as a
market order and can fill far worse on a gap.

**Fill** (`market.Fill`) — an executed quantity at an executed price. Orders are
instructions; fills are facts.

**Partial fill** — a fill smaller than the order, because the book showed less
size than was asked for.

**Gap** — the market opening beyond a level without trading through it. The
reason a stop can fill much worse than its level.

**Long / short** — a position that gains when the price rises / falls.

**Position** (`portfolio.Position`) — a net holding. `NetQty` is signed;
`CostBasisCts` is exact committed cash.

**Cost basis** — the exact signed cash committed to a position. Not an average
price; averages lose precision on every partial fill.

**Weighted average cost (WAC)** — closing part of a position removes a
proportional share of its cost basis, rather than tracking individual lots
(FIFO).

**Realised P&L** — money from closed quantity. Proceeds minus the cost removed.

**Unrealised P&L** — what an open position would realise at a given mark.

**Mark** (`portfolio.Mark`) — the price at which an open position is valued.

**Flip** — a fill that reduces past flat: a close and a new open, two facts,
commission on both.

**Equity** — `starting + realised − fees + unrealised`. What challenge rules are
measured against.

**Flat** — a position of zero. It keeps existing; it is not deleted.

**Commission** — per-contract cost of trading. In Praxis a flat figure on the
account.

**Slippage** — the difference between the price you expected and the price you
got. In Praxis it is never favourable.

**LogicalTime** (`market.LogicalTime`) — nanoseconds since the Unix epoch, UTC.
The domain's only notion of time, and it always arrives as data. There is no
`time.Now()` anywhere in the domain.

**SessionID** — an identifier stamped by the adapter. A change of identifier is
a session boundary. The kernel compares identifiers and never reads a calendar
(ADR-011).

**Session** — a trading day as the venue defines it, not a calendar day. CME
equity index futures run 17:00 to 16:00 US Central.

**Prop firm** — a firm that funds traders who pass an evaluation.

**Evaluation / challenge** — a simulated account with hard rules: profit target,
daily loss limit, drawdown, contract limits.

**Daily loss limit** — lose more than this within one session and the evaluation
fails.

**Static drawdown** — a fixed floor under the account.

**Trailing drawdown** — a floor that rises with the account's high-water mark
and never falls. Where most evaluations actually end.

**High-water mark** — the highest equity the account has reached.

**Determinism** — the same data, actions, seed and configuration produce
byte-identical results. Every other guarantee in Praxis depends on it.

**Property test** — a test asserting an invariant over many generated inputs,
rather than one example. "A buy never fills below the ask" is one.

**Mutation testing** — deliberately breaking the implementation to check that a
test notices. A test that survives every mutation is proving nothing.

**Ambiguous bar** — a bar that reached both stop and target without saying in
which order. Praxis resolves it to the stop and records the ambiguity as a fact
about the observation.

**Rollover** — moving a position from an expiring contract to the next one. Not
modelled in Praxis.

**Margin** — the deposit required to hold a futures position. Not modelled in
Praxis.

# Praxis — Engineering Specification

This is the normative source for product reasoning, settled domain decisions,
and the roadmap. `AGENTS.md` contains the always-on working rules and wins if
the documents conflict. `docs/PRAXIS_CONTEXT_ES.md` is a non-normative Spanish
summary, and `docs/learning/` is a non-normative introduction to the trading
domain for a reader new to it, with a glossary.

## 1. Product thesis

Praxis is a deterministic trading simulator for measuring and training trader
behaviour. It replays real or generated market sessions, applies configurable
prop-firm-style rules, and records meaningful decisions as domain events—not
only trade outcomes.

The long-term loop is:

```text
weakness detected -> targeted scenario -> user trades -> behaviour measured
  -> model updated -> next drill prescribed
```

Adaptive training is not initial scope. The first goal is an exceptional,
deterministic simulation kernel.

Out of scope: auth, billing, subscriptions, multi-tenancy, marketing site,
social features, leaderboards, mobile, ML, LLM coaching, blockchain, Kafka,
Kubernetes, microservices, real-money execution, broker integration, trading
signals, market prediction, and hardcoded provider-specific challenge engines.

## 2. Repository state

At the time this specification was installed, the repository contained only
`README.md`. Earlier design material described a planned baseline containing
`domain/types.go`, `domain/execution.go`, and 24 passing tests; that baseline
was never present in this repository.

What exists now, built and verified here. `internal/market` holds the observed
market vocabulary—`Ticks`, `Cents`, `Instrument` with its `CentsPerTick`,
`Quote`, `Bar`, `Order`, `Fill`—and checked arithmetic. `internal/execution`
holds `ConservativeExecution` for market, limit and stop orders on a quote, and
`WorstCaseIntrabar` for a completed bar. `internal/portfolio` holds `Position`
with an exact cost basis and `Account`, which applies fills and reports equity.
`internal/challenge` holds a `Challenge` state machine applying a static daily
loss limit across explicit session boundaries.

There is no event store, no aggregator, no generic market event and no adapter.

## 3. Settled decisions

### ADR-001 — Go for the kernel

Use Go for the kernel, TypeScript for a later UI, and Python only when dedicated
statistics work exists. Determinism comes from logical time, explicit ordering,
and controlled inputs—not from choosing Rust. Reconsider Rust only for a real,
profiled hot path.

### ADR-002 — Integer monetary arithmetic

Binary floating-point cannot exactly represent many monetary values and makes
rounding rules easy to apply inconsistently. Domain prices, money, P&L, fees,
and risk values therefore use defined integer types such as `Ticks` and
`Cents`. Floating-point is allowed only at explicitly marked analytics or
random-sampling boundaries and cannot feed monetary or risk logic.

### ADR-003 — Logical time

`LogicalTime`—nanoseconds since the Unix epoch in UTC—is the domain's only
notion of time. Replay advances it from data; live paper advances it from the
feed. The kernel cannot distinguish the source.

### ADR-004 — Conservative execution

Execution never gives the trader an unearned advantage. Market orders cross
the spread. Limit orders receive no favourable price improvement. Stops may
slip through their level on gaps but never improve on it. If stop and target
are both reachable in a bar and their order is unknown, the stop wins.

### ADR-005 — Modular monolith with hexagonal boundaries

Domain code is isolated from infrastructure. Split modules only when a vertical
slice has enough complexity to justify it. No microservices or distributed
event-sourcing platform.

### ADR-006 — One ordered event model

Replay, synthetic, file, and live-paper sources produce the same domain market
events. Events entering the kernel are ordered by `LogicalTime`; equal times
preserve source sequence using an explicit monotonic sequence number. Adapters
must reject or deterministically normalize out-of-order input.

### ADR-007 — Intrabar resolution is policy

`Bar` describes market observations. Ambiguous-bar resolution is a simulation
policy. Future tick or lower-timeframe policies may be added without changing
`Bar`, but not before they are required.

### ADR-008 — Behavioural event sourcing

Praxis records decisions and their contemporaneous domain context, not merely
trades. The store is append-only. Behavioural interpretations such as revenge
trading belong to analytics, not execution.

### ADR-009 — MNQ is the initial instrument

MNQ is settled for the first product slice. The architecture may support more
instruments later, but the product does not expose them yet. Crypto may provide
cheap development data through an adapter; it must not distort the kernel
around crypto semantics.

### ADR-010 — Bars are adapter-supplied first-class observations

`Quote` and `Bar` are distinct observations and a session executes at exactly
one resolution, never both. Bars come from an adapter, which either reads
vendor bars or aggregates a more granular source deterministically; the kernel
never builds bars, because choosing bid, ask or midpoint as the basis silently
changes OHLC and therefore changes which stops trigger. Provenance lives in the
session and source configuration. See
[`docs/adr/010-bars-are-adapter-supplied-observations.md`](adr/010-bars-are-adapter-supplied-observations.md)
for the full decision, including bar semantics, ordering and gap rules.

### ADR-011 — Session boundaries arrive as data

The adapter stamps every observation with a `SessionID` and the kernel compares
identifiers for equality, never reading a clock, a calendar or a time zone. A
session begins with an explicit `SessionOpened` carrying its reference equity,
never by inferring a boundary from the first ordinary snapshot; a snapshot for
a session that was never opened is rejected. The reference is equity, so it
includes unrealised P&L and fees. Positions open across a boundary are not
liquidated. Inputs must be strictly increasing in `(LogicalTime, Sequence)` and
a `SessionID` never returns. See
[`docs/adr/011-session-boundaries.md`](adr/011-session-boundaries.md).

## 4. Domain rules

### Exact cost basis

`Position` stores an exact `CostBasisCts Cents`, not a rounded integer average
entry price. For an instrument whose tick value is an integer number of cents:

```text
fill cost = fill price in ticks * instrument cents per tick * absolute quantity
```

`CentsPerTick` belongs to immutable instrument specifications; it is never a
global implicit constant. Reject an instrument whose monetary tick value cannot
be represented exactly by the configured integer unit. `AvgPx()` is derived for
display only and must never feed P&L or risk calculations.

Partial closes use weighted-average cost, not FIFO. Allocate proportional cost
deterministically. If division is unavoidable, realised P&L rounds toward the
loss; fees round up. Document the exact integer division rule in tests.

The exact rule: the cost removed by a partial close is
`ceil(costBasis * closedQty / openQty)`, rounded toward positive infinity. That
direction reduces realised P&L for a long and for a short alike, so an inexact
allocation always costs the trader. The residual basis is what remains after
subtracting the allocation, never a figure computed independently, so the
allocations over any sequence of partial closes sum to exactly the original
basis and no cent is created or lost.

A flip is a close followed by a new open. Long 3 to short 2 via a sell of 5
charges commission on all five contracts, realises the closed three, and starts
a clean short cost basis for two.

A zero-quantity position remains as a flat position associated with its
instrument and emits `PositionClosed`; it is not deleted mid-session.

### Limit orders fill at their limit price

This is a deliberately pessimistic simulation policy, not a claim about market
microstructure. A real resting limit order can be filled at a better price than
its limit. Praxis never grants that, because unearned price improvement teaches
a habit the market will not honour. It is recorded here so that it is not later
"corrected" as a bug by someone who expects a fill at the ask or the bid.

- A buy limit is executable when the ask is at or below the limit, and fills at
  the limit.
- A sell limit is executable when the bid is at or above the limit, and fills at
  the limit.
- No fill ever occurs outside the limit price.
- Filled quantity is capped by the size displayed on the taken side, as for
  market orders.
- A limit that is not executable against the observation produces no fills and
  no error.
- A limit price is part of a valid limit order; an order of that type without
  one is invalid input, not an order that never executes.

### Stops fill at the touch, not at their level

A stop is a trigger, never a promised price. It is triggered when the touch on
the taken side has reached the level, and then executes as a market order.

- A buy stop triggers when the ask is at or above the level, and fills at the
  ask.
- A sell stop triggers when the bid is at or below the level, and fills at the
  bid.
- A gap therefore fills worse than the level, sometimes far worse; it never
  fills better than the level.
- Triggering does not imply a fill: an empty side produces no fills.
- An untriggered stop produces no fills and no error.
- A stop price is required on a stop order and forbidden on any other type, as
  a limit price is required on a limit order and forbidden on any other.

The trigger comparisons mirror those of a limit, but the domain meanings are
opposites—a limit waits for the market to come to it, a stop waits for the
market to move into it—so they are written as separate predicates rather than
shared behind a flag.

### A bar's open settles what it can

Worst-case intrabar resolution treats a position's protective levels as orders
on the exit side: the stop is a stop order and the target is a limit order, so
whether a level was reached is decided by the same predicates as quote
execution, applied to the extreme of the bar that could have reached it.

The open is the first price the bar showed. A level the open is already beyond
was reached before anything else in the interval, so such a bar is not
ambiguous: the stop fills at the open, worse than its level, and a target
gapped through still fills at the target and never better. `Ambiguous` is a
fact about the observation, not about the policy, and recording it when the
open already settled the order would put a false fact in the behavioural log.

Only when the open sits between the levels and the bar later reaches both is
the order of events unknowable. Then the stop wins.

### Losses read equity, gains read balance

An account valuation has two figures and the rules disagree about which they
mean. Every input to the challenge engine carries both, taken atomically from
the same account at the same mark, and the engine infers neither.

- Daily loss, static drawdown and trailing drawdown read **equity**. An open
  loss must be able to end an evaluation immediately.
- The profit target reads **balance**: starting balance plus realised P&L,
  minus fees. Money passes an evaluation once it has been realised, not while
  it is still on the screen.

The asymmetry is the conservative principle applied to both ends. Reading
equity for the target would let a position that touches the target for an
instant and gives the whole gain back buy an irreversible approval — a
momentary swing converted into a permanent pass, decided in the trader's
favour. Reading balance for the losses would let an unbounded open loss sit
unmeasured.

Commissions reduce progress toward the target because they are already in the
balance; no separate rule is needed.

### A daily loss breach beats the profit target

The daily loss limit is measured against the open session's reference; the
profit target against the equity the evaluation began with, which no boundary
moves. Because the two references differ, one snapshot can breach both: a
session that opened after a large run-up can be far enough down on the day to
fail while the evaluation is still far enough up to pass.

That state is reachable with entirely coherent rules, so the precedence is
decided rather than left undefined, and the loss wins. A simulator must never
resolve an ambiguity in the trader's favour.

Reaching the target exactly passes; losing exactly the daily limit does not
fail. The asymmetry is deliberate and follows the same principle.

### A symbol is not an instrument

Within one account a symbol names exactly one monetary specification. A fill or
a mark carrying a known symbol with a different `CentsPerTick` is rejected, and
the account is left untouched: accepting it would accumulate two incompatible
cost bases in one position and manufacture P&L with no price movement at all.

### Prices are strictly positive

Praxis rejects prices at or below zero on fills, quotes, bars, order levels and
marks. This is a decision scoped to MNQ, the only supported instrument, and it
is what makes the cost-basis sign relation—a long commits cash, a short
receives it—an invariant rather than a coincidence. An instrument that really
trades at or below zero cannot simply be added; both claims must be revisited
together.

### Overflow is an error, not a wrap

Go's integer overflow is silent, so exact accounting is only exact until it
happens. Every monetary and quantity operation goes through checked arithmetic
that reports `ErrOverflow`. `ApplyFill` computes the whole new state into local
values first and writes the account only once all of it has succeeded, so a
rejected fill leaves position, realised P&L and fees exactly as they were.

### A position is flat exactly when it holds no cost

`NetQty == 0` if and only if `CostBasisCts == 0`, and an open position's basis
sign matches its direction. `Position.Validate` enforces both and `ApplyFill`
checks the result before committing.

### Domain values validate themselves

`Order` and `Quote` have exported fields, so a constructor cannot make an
invalid value unrepresentable: any caller can compose one directly. Validity
therefore lives on the value as a `Validate` method that every consumer calls,
so a constructor and its consumers cannot drift apart. Consumers report the
class of fault they are rejecting and wrap the precise cause.

## 5. Explicit port exceptions

Do not introduce an interface before a second implementation exists, except
for these genuine boundaries:

```go
type MarketDataPort interface {
    // Stream emits events in ascending (LogicalTime, Sequence) order.
    // It reports initialization errors directly. The concrete streaming
    // result must also represent errors that occur after streaming begins.
    Stream(i Instrument, from, to LogicalTime) (<-chan MarketDataResult, error)
}

type ExecutionPolicy interface {
    ExecuteOnQuote(o Order, q Quote) ([]Fill, error)
}

type IntrabarResolutionPolicy interface {
    // Reports an error for input that cannot describe a resolution, as
    // ExecutionPolicy does. Bar, Position and ProtectiveLevels all have
    // exported fields, so a consumer cannot assume a constructor was used.
    Resolve(bar Bar, pos Position, lv ProtectiveLevels) (IntrabarResult, error)
}

type EventStorePort interface {
    Append(sessionID string, events []DomainEvent) error
    Load(sessionID string) ([]DomainEvent, error)
}

type Randomizer interface {
    Int63n(n int64) int64
    NormFloat64() float64 // sampling boundary only
    Seed() int64
}
```

`MarketDataResult` is intentionally not specified until the first adapter
slice defines its required event/error semantics. Do not scaffold it early.

## 6. Domain boundaries

Execution produces fills and knows nothing about Topstep, FTMO, Apex, or any
other provider. Challenge consumes fills and applies `ChallengeRules` values;
there are no `if provider == ...` branches.

The only aggregate candidates currently accepted are `Account`, `Challenge`,
and `TradingSession`. Not every entity is an aggregate.

## 7. Testing policy

Use table-driven unit tests, property/invariant tests, deterministic replay
tests, and a regression test for every confirmed bug. Do not add a Gherkin
runner. Business-readable scenarios may be comments above table tests.

Required growing properties:

- Same data, actions, seed, configuration, and ordered input produce identical
  fills, event streams, and final account across 20 runs.
- Execution never produces an impossible favourable fill.
- The same ambiguous bar always resolves identically.
- A trailing drawdown threshold can rise with its high-water mark and never
  moves down.
- Cost basis is conserved across partial closes: for any partition of a
  position into partial closes at the same prices, total realised P&L equals
  the realised P&L of a single full close. Achieve this by removing an exact
  allocated cost from the basis and leaving the remainder in the residual
  basis, never by computing the remaining basis independently. If a future
  rule makes exact conservation impossible, state the accepted leakage bound
  instead of leaving it undefined.

## 8. Roadmap

### Phase 0 — Execution kernel

Implement conservative quote execution, worst-case intrabar resolution,
determinism, and property tests. This phase is planned, not complete in the
current repository.

### Phase 1 — Position and Account

Implement exact cost basis, opening and adding both sides, partial and full
closes, flips, realised and unrealised P&L, accumulated commissions, and account
equity. Exit: 100 known fills produce the same state across 20 runs and match an
independently calculated expected result.

### Phase 2 — Challenge engine

Implement `Pending -> Active -> Passed | Failed` with invalid transitions
unrepresentable. Rules are configuration. Cover static and trailing drawdown,
unrealised breaches, high-water marks, session-boundary resets, positions across
boundaries, time zones, contract limits, minimum days, and consistency rules.

### Phase 3 — Behavioural event log

Add append-only persistence and a minimal replay CLI. Exit: reconstruct a full
session from its event log without information loss. No web UI.

### Phase 4 — Personal experiment

Run sessions against hypotheses frozen before session one. An effect counts
only if it is present in both halves of the sample. Derive the session target
from a power calculation performed before freezing the hypotheses—trades per
session, expected frequency of the conditioning event, and assumed dispersion
of the outcome—rather than from a round number. Splitting the sample in half
halves the power of each test. Declare one primary hypothesis; the rest are
exploratory, because four tests at p < 0.05 carry roughly a 19% family-wise
error rate. If no stable
behavioural patterns appear, stop adaptive-training work and retain Praxis as a
personal simulator.

### Phases 5–7

Only after Phase 4 supports the thesis: build a synthetic generator calibrated
against measured real distributions, then adaptive training, then live paper
through the same event model. Validation always uses unseen real sessions.

## 9. Project structure

Let structure emerge through vertical slices. Do not scaffold this tree in
advance:

```text
cmd/praxis/
internal/
    market/ execution/ portfolio/ challenge/ session/ journal/
    adapters/marketdata/ adapters/persistence/
docs/adr/ docs/hypotheses.md
testdata/
```

No packages named `utils`, `common`, `helpers`, or `manager`. Use constructors
when they enforce invariants and avoid setters that permit invalid states. Wrap
errors when context matters, never swallow them, and do not panic for expected
domain failures.

## 10. Next smallest vertical slice

Phases 0 and 1 are complete and hardened, and Phase 2 has begun. The challenge
engine applies two rules: a static daily loss limit measured against a session
reference that arrives with an explicit `SessionOpened`, and a profit target
measured on balance against the balance the evaluation began with. The state machine is now
terminal in both directions.

The next rules, one slice at a time and in this order:

1. **Static maximum drawdown**, a fixed floor under the account.
2. **Trailing drawdown**, which is where the real difficulty lives: the
   threshold rises with the high-water mark and never moves down, and
   unrealised equity can breach it with no trade closed.
3. **Contract limit**, **minimum trading days**, **consistency rule**.

Known gaps to close when a slice needs them: commission is a flat per-contract
figure on the account, not a schedule and not per instrument; nothing records
the behavioural context around a fill, which is Phase 3.

Nothing produces `AccountSnapshot` values, and that is deliberate. It belongs
to none of the packages that exist: `portfolio` knows nothing of sessions,
`challenge` must not reach into an account, and a market data adapter must not
own one. It is composed by a future application layer—the sessionizer supplies
`SessionID`, `Time` and `Sequence`, the account supplies `EquityCts`—and
building that layer now would mean inventing orchestration before an event log
or a CLI exists. Until then the integration tests compose it by hand, which
documents the boundary honestly.

## 11. Statistical and commercial guardrails

Freeze hypotheses before collecting sessions; do not rewrite them during data
collection. An underpowered study fails to reject for lack of data, not for
absence of effect: a kill criterion evaluated without a prior power
calculation can retire a true thesis. With a small sample, exploratory patterns are hypotheses, not
findings. Stability across two halves is a minimum guardrail, not proof of
causality or transfer to real-money trading.

Do not build commercial features before completing the personal experiment.
Never market P&L or imply expected returns. Data licensing and financial
promotion requirements are time-sensitive legal questions and must be verified
with the relevant provider and qualified counsel before commercial use.

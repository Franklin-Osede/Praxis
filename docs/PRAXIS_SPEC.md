# Praxis — Engineering Specification

This is the normative source for product reasoning, settled domain decisions,
and the roadmap. `AGENTS.md` contains the always-on working rules and wins if
the documents conflict. `docs/PRAXIS_CONTEXT_ES.md` is a non-normative Spanish
summary.

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
`domain/types.go`, `domain/execution.go`, and 24 passing tests, but that baseline
is not present and has not been verified in this repository.

Always inspect the repository and run the suite before relying on a documented
baseline. Never rewrite working code without evidence that it is wrong.

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

A flip is a close followed by a new open. Long 3 to short 2 via a sell of 5
charges commission on all five contracts, realises the closed three, and starts
a clean short cost basis for two.

A zero-quantity position remains as a flat position associated with its
instrument and emits `PositionClosed`; it is not deleted mid-session.

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
    Resolve(bar Bar, pos Position, lv ProtectiveLevels) IntrabarResult
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

Because the documented Phase 0 implementation is absent, the next slice is the
smallest coherent Phase 0 execution path—not Position/Account. Before coding,
define the domain types needed by one conservative market-order-on-quote case,
write its invariant tests, implement the minimum, and grow Phase 0 case by case.

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

# ADR-011 — Session boundaries arrive as data

Status: accepted.

## Decision

A trading day is not something the domain calculates. The adapter stamps every
observation it emits with a `SessionID`, and a change of `SessionID` is a
session boundary. The kernel receives the identifier as data and compares it
for equality; it never reads a clock, never holds a venue calendar and never
knows a time zone.

```go
type SessionID string
```

Time zones, venue calendars, holidays, half sessions and the choice of when a
day begins all live in the adapter's source configuration, and the provenance
recorded in `SessionStarted` must be enough to reproduce the same stamping.

## Reasons

`LogicalTime` already exists and is enough to order observations, but it cannot
answer "is this the same trading day". CME's equity index session opens at
17:00 US Central and runs to 16:00 the following afternoon, so a trading day
and a calendar day are offset by seven hours, plus whichever daylight-saving
rule applies that week. Resetting a daily limit at midnight would reset it in
the middle of a live session.

Putting that calendar in the domain would mean the kernel knows about one
venue, which ADR-005 and the challenge engine's provider independence both
forbid. Putting it in the adapter means a synthetic session, a replayed session
and a live session are all just observations carrying identifiers, and the
kernel cannot tell them apart—which is the test ADR-006 sets for the design.

## What the challenge engine consumes

The challenge engine does not reach into an account. It consumes two inputs on
one ordered stream:

```go
type SessionOpened struct {
    Time               market.LogicalTime
    Sequence           uint64
    SessionID          SessionID
    ReferenceEquityCts market.Cents
}

type AccountSnapshot struct {
    Time      market.LogicalTime
    Sequence  uint64
    SessionID SessionID
    EquityCts market.Cents
}
```

This keeps the dependency one-way. Portfolio produces values; challenge applies
rules to them and decides consequences.

## A session opens explicitly

A session begins with a `SessionOpened`, never by inferring one from the first
ordinary snapshot that happens to carry a new identifier.

Inferring it conflates two different facts—"the market was observed" and "a
trading day began"—into one input, so a defect that loses the first snapshot of
a session would silently re-base the reference against a later, different
equity, and nothing in the stream would record that it happened. An explicit
input makes the boundary a fact the adapter asserts and the event log can
carry.

A snapshot whose `SessionID` was never opened is rejected. The kernel does not
open a session on its behalf.

## Reference equity

The reference equity is stated by the `SessionOpened`, and it is equity, not
realised P&L: `starting + realised − fees + unrealised`. A position held across
a boundary therefore carries its open P&L into the new session's reference,
which is what an evaluation measuring intraday drawdown does.

The challenge engine sees one number and cannot tell which part of it came from
unrealised P&L or from commissions. That both are included is a property of how
equity is computed, and it is proven where it is decided—in an integration test
that builds a real account—not by a unit test that would only be restating its
own input.

## Open positions at a boundary

Nothing is liquidated. A position open when the session changes stays open,
unchanged in quantity and cost basis. Only the reference re-bases.

Automatic liquidation at a session close is a separate rule with its own
consequences for fills and fees. It is not part of this decision and must not
be smuggled into it.

## What counts toward a daily limit

Both commissions and unrealised P&L count, because the limit is measured on
equity and equity contains both.

Counting only realised P&L would let a trader hold an unbounded open loss and
never fail—the precise behaviour Praxis exists to measure. Excluding fees would
let an account fail in reality while passing in simulation.

## Ordering and identity

Both inputs share one ordering. Each must be **strictly increasing** in
`(LogicalTime, Sequence)` against everything the kernel has already accepted.
An out-of-order input is rejected, not sorted: sorting would hide an adapter
defect that changes results. A repeated `(LogicalTime, Sequence)` pair is
rejected too—`Sequence` exists precisely so that two inputs at the same logical
instant can be told apart, and two inputs at the same position are a delivery
defect.

A rejected input changes nothing about the challenge's state.

A `SessionID` never returns. Once the kernel has moved past a session, a
snapshot carrying that identifier again is an adapter defect and is rejected.
Without this rule a repeated identifier would silently reset a daily limit,
which is indistinguishable from a trader being handed a fresh allowance.

Identifiers are compared for equality only. The kernel must not parse them,
order them or infer a date from them, so an adapter is free to use any stable
scheme.

## Consequences

Two adapters that disagree about when a session begins will produce different
pass/fail outcomes from identical market data. That is a real risk, and it is
why the session scheme is part of the provenance recorded for a run rather than
an implicit convention.

A session that is never closed—a replay that ends mid-session—leaves the last
reference in place. Ending a run is not a boundary, and the challenge engine
must not treat the end of a stream as one.

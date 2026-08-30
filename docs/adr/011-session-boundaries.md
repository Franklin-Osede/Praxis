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

The challenge engine does not reach into an account. It consumes snapshots:

```go
type AccountSnapshot struct {
    Time      market.LogicalTime
    SessionID SessionID
    EquityCts market.Cents
}
```

This keeps the dependency one-way. Portfolio produces a value; challenge
applies rules to it and decides consequences.

## Reference equity

A session's reference equity is taken from the **first snapshot carrying the
new `SessionID`**, before any order in that session executes.

It is equity, not realised P&L: `starting + realised − fees + unrealised`. A
position held across a boundary therefore carries its open P&L into the new
session's reference, which is what an evaluation measuring intraday drawdown
does.

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

Snapshots reaching the kernel must be non-decreasing in
`(LogicalTime, Sequence)`. An out-of-order snapshot is rejected, not sorted:
sorting would hide an adapter defect that changes results.

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

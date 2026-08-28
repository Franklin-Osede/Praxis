# ADR-010 — Bars are adapter-supplied first-class observations

Status: accepted.

## Decision

`Quote` and `Bar` are distinct market observations accepted by the simulation
kernel. A trading session selects exactly one execution resolution:

- quote resolution, or
- bar resolution.

The kernel never executes the same order from both streams in one session.

Bars are supplied by an adapter. An adapter may:

1. read venue or vendor bars directly, or
2. aggregate a more granular source through a deterministic aggregation
   component.

Aggregation is outside the domain kernel. The aggregation algorithm may be
shared by replay and live adapters.

## Reasons

Aggregating top-of-book quotes does not produce bars of real trades. Choosing
bid, ask or midpoint as the basis changes OHLC and therefore changes which
stops trigger; there is no neutral choice for the kernel to make silently.

The kernel should not know about windows, time alignment, venue sessions or
incomplete data. Those are properties of a source.

Replay/live equivalence means the same domain observations in the same order,
not that the kernel reconstructs bars for itself.

Mixing quote and bar execution in one session would allow the same order to
execute twice and would force the kernel to order observations of different
granularities against each other.

Provenance can be recorded without contaminating `Bar` with execution policy.

## Bar semantics

A `Bar` contains:

- instrument;
- interval start and end as logical time;
- open, high, low and close in ticks;
- optional volume, only when its meaning and source are defined;
- source sequence for deterministic ordering.

The interval convention is `[start, end)`. Bars are emitted in ascending
`(EndTime, Sequence)` order.

A bar must satisfy:

- non-empty instrument;
- `StartTime < EndTime`;
- `Low <= Open <= High`;
- `Low <= Close <= High`;
- non-negative volume when present.

## Provenance

Session configuration and `SessionStarted` record enough provenance to replay
the observations:

- provider or dataset identifier;
- source kind: vendor bar or aggregated;
- interval;
- price basis: trades, bid, ask or midpoint;
- session and time-zone alignment;
- aggregation algorithm version;
- dataset version or immutable content digest when available.

Provenance belongs to the session and source configuration, not repeated inside
every `Bar`.

## Ordering

Quote and bar observations may share an event envelope in infrastructure, but a
session cannot use both as execution inputs, so cross-type ordering does not
affect fills.

If mixed observations are retained for diagnostics, they are ordered by
`(LogicalTime, Sequence)` and only the configured resolution is routed to
execution.

## Gap semantics

For adapter-supplied bars, a gap exists when the current bar opens beyond a
protective level relative to the previous accepted observation.

Execution uses the current bar's `Open` as the first executable price:

- a sell stop gapped below fills at `Open`, never at a better stop price;
- a buy stop gapped above fills at `Open`, never at a better stop price.

The kernel does not infer unseen paths between bars.

An aggregated bar can expose only movements present in its underlying source.
It must not synthesize a venue gap absent from that source.

## Consequences

Vendor bars and locally aggregated bars are not assumed equivalent unless their
price basis, interval alignment and aggregation rules match.

Replay/live equivalence is tested using the same ordered observation sequence
and configuration. It is not claimed merely because both sources produce a
`Bar`.

The event log stores the accepted bar observations, or references immutable
source data sufficient to reproduce them exactly.

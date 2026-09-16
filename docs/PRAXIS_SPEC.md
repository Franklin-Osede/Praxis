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
market vocabulary and checked arithmetic. `internal/execution` holds
`ConservativeExecution` for market, limit and stop orders on a quote, and
`WorstCaseIntrabar` for a completed bar. `internal/portfolio` holds `Position`
with an exact cost basis and `Account`. `internal/challenge` holds a state
machine applying a daily loss limit, a static drawdown floor, a trailing
drawdown and a profit target. `internal/session` composes all of them into one
deterministic run with an ordered in-memory journal, proves that journal
against the aggregates that produced it, and resumes a whole session from it.

`internal/adapters/marketdata` reads a versioned canonical file into ordered
observations and drives a session with them, and
`internal/adapters/persistence` speaks three payload versions, encodes a
journal as canonical text, frames it
into checksummed batches, reads it back, appends to it under an exclusive
advisory lock with an explicit durability policy, and recovers the session
state its confirmed batches describe. A session commits one batch per command
through a narrow port and stops for good if a commit fails.

`cmd/praxis` is the command: `praxis replay` runs or resumes a simulation over
a market file; `praxis store inspect` checks a journal's frames, `praxis store
verify` proves its history against the aggregates that produced it, and
`praxis store repair` mends an unconfirmed or damaged tail. Exit codes are meant to be automated
against.

**What that path does not yet include.** `praxis replay` feeds observations and
nothing else: no order can be submitted except from Go code, `SubmitOrder` has
no caller outside tests, and the byte-identical resume proof runs over a
fixture containing no orders at all. A human can therefore produce a journal of
a market and its evaluation, and cannot yet produce a journal of a decision —
which is the only kind Phase 4 needs.

Two further gaps follow from that. An order that does not fill against the
current observation is dropped rather than rested, so the unfilled remainder
`ExecuteOnQuote` documents as "the caller's to carry" is not carried by its only
caller. And `WorstCaseIntrabar` and `Bar` are complete and tested but reachable
from nothing outside `internal/execution`, because no adapter supplies a bar —
correct under ADR-010, and not a capability.

There is no UI.

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

### ADR-012 — A command is the atomic unit of the event store

The event store frames a whole command's events as one batch — a text header of
fixed-width twenty-digit fields carrying the batch number, the payload's byte length, its first and
last event sequence and its event count, a CRC32C computed over that metadata
as well as the payload, then a canonical text payload; the file names its
container and payload versions separately, because they change for different
reasons — confirmed together or not at all, because per-event framing would
leave a command cut in half looking perfectly intact. One writer per journal,
checksums for accidental corruption and not for tamper resistance, and a reader
that accepts only complete, continuous batches and never repairs. Integrity and
durability are separate properties with separate names. A failed write is
ambiguous, so recovery discovers whether the batch landed and never re-executes
the command; state is rebuilt from the confirmed batches rather than from a copy
of the aggregates. See
[`docs/adr/012-event-store-batches.md`](adr/012-event-store-batches.md).

### ADR-013 — A trade is a position episode

A trade is the span during which the net position in one instrument stays
non-zero and keeps its direction; additions and partial exits belong to it, a
flip ends one and opens another, and neither a session boundary nor the end of
the data nor a terminal challenge ends it. Its identifier is the journal
sequence of the `PositionChanged` event that opened it — deterministic, and
needing no registry. Its result is realised P&L minus every leg's commission,
and break-even includes fees, because an episode that gave back its gain in
commission was not economically flat. An open episode has no result at all.

One trade per entry order was rejected: weighted average cost means the system
does not know which entry an exit closed, and choosing FIFO or pro rata would
invent that knowledge. A user-declared campaign was rejected as a primary unit
because it can be relabelled after the fact.

`OrderContext.ConsecutiveLosses` counts closing legs and is not renamed —
that would change what already-written journals say about their own past. A
hypothesis about losing streaks needs `ConsecutiveLosingTrades`, a second
counter advanced only when an episode ends. Both counters and the protection
events inaugurate `praxis.event.v2`: v1 keeps the schema it has, because a
version is stable when its bytes stop changing rather than when anyone promises
the next change will be the last. See
[`docs/adr/013-position-episodes.md`](adr/013-position-episodes.md).

### ADR-014 — Protection is an aggregate, not two prices

A protective level has an identity, a quantity and a lifecycle of its own.
Treating it as a pair of numbers on something else produced a schema its own
producer could not honour: a planned protection could be neither moved nor
withdrawn while its entry waited, a protective fill had no order to belong to,
and `Executed` was terminal when a partial fill leaves exposure standing.

References are a tagged union naming either the entry or the episode, never a
zero episode used as a sentinel. Each leg is an order named from a reserved
`praxis:` namespace keyed by the event's own sequence, recorded explicitly and
reproducible on replay, and **no identifier is ever reused in a journal** —
refusing only collisions with orders currently working would let a finished
name come back and make grouping fills by decision ambiguous again. Exactly one
protection governs an episode: a plan that fills into an already protected
position does not activate, it ends with a stated reason and the existing
quantity grows. The machine
is `Planned → Active(stop?, target?, quantity) → Ended`, and execution is a
reason for a transition rather than a state.

`praxis.event.v3` is published together with the commands that write it: an
entry and its levels are submitted as one command and therefore one durable
batch, and a planned protection can be changed or withdrawn while its entry
waits. Within that batch the protection is recorded **between the decision and
its first fill**, because activation binds a plan to what the fill actually did
and a plan written afterwards could only be tied to it by inferring causation
backwards from ordering. **A plan never outlives its entry**: cancelling the
entry — by the trader, or because a market order's remainder could not fill —
ends the plan in the same batch, in that order, and `Replay` and `Verify` both
refuse a journal in which anything else stands between the two.

**Activation is derived, never recorded**: a plan becomes active because a
position change already in the log opened the exposure it was placed for, and
an event saying so would be a second copy of a fact. A fill's changes are
assembled before any of them is written, because a reversal is one fill closing
one position and opening another, and a decision taken on the close alone would
end the arriving plan as having opened nothing one event before the exposure it
opens. Only an event ever removes a protection — folding a change never does —
which is what lets `Verify`, holding the events but not the fills, reach the
same state as `Replay`, holding both. The two legs must also be on the right
sides of each other, or they do each other's job and both can be reachable in a
single observation.

**A protection meets the observation that activated it**, not the next one: an
entry that filled through a gap may already be past its stop. Within one
observation the protections already standing go first, then each working order
in turn with its own protection resolved before the next order is offered
anything — otherwise the next order takes the liquidity that stop should have
found. Inside a protection the stop goes before the target, because without a
real queue position nothing says which of two reachable levels the market took
first, and choosing the target would hand the trader the better of two outcomes
the engine cannot know.

Every leg ends with an event. A partially filled stop has its remainder
cancelled — it triggered and cannot untrigger — while a partially filled target
leaves both legs standing over what is left. A leg that closes the position
cancels its sibling `by_oco` and ends the aggregate `executed`; a manual exit
cancels both `position_closed`. The two reasons are distinguished because a log
that spelled them alike could not tell a stop that worked from one the trader
overtook. They are values no earlier version has a name for, so they inaugurate
`praxis.event.v4`, published with the commands that first write them — along
with **who traded the journal**, because the experiment's unit of analysis is a
trader and not a session, and a field added after the first recorded session
would be a payload version with a migration behind it. It is a label the
protocol assigns, never a person's name, and it is empty for a journal nobody
traded.

Every part of a protective tail is recomputed rather than believed, including
the one transition that leaves no position change: a stop that reached its level
and found nothing. `Replay` re-executes the leg against the book as it stood and
demands the level was reached, that nothing was left on it, and that the whole
cover was withdrawn. That needs the book as it stood, so **`Replay` consumes the
displayed size from the fills the journal records**, exactly as a live session
does — without which a resumed session would inherit the quote as it arrived
rather than as it was left, and could trade depth the interrupted run had
already spent. A trader's own cancelled remainder is still believed: the book
when it was written is not the book its fills met.

The protection events in v2 were defined ahead of any producer, and
that is precisely what let them be wrong: a schema is not proven until
something both produces and consumes it. See
[`docs/adr/014-protection-is-an-aggregate.md`](adr/014-protection-is-an-aggregate.md).

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

### An evaluation activates at its configured starting valuation

`Rules.StartingBalanceCts` is the valuation an evaluation is contracted to
begin at, and activation is rejected unless the opening balance and equity both
equal it. The static drawdown floor and the profit target are anchored to that
configured figure, so neither depends on a mark taken at the instant of
activation—a floor derived from an incidental observation would be a permanent
consequence of a momentary one.

This does not prove the account holds no position: a position at break-even has
zero unrealised P&L and satisfies the check. It proves only that the evaluation
began where its rules say it began. The session layer must open a challenge
immediately after creating the account and before accepting any order.

### Events carry the whole valuation

A domain event records both the balance and the equity that produced its
decision, never one field whose meaning depends on the event's kind. A
persisted behavioural log must not have to reinterpret a value to know what it
holds, and a reader must be able to see that a failure was decided on equity
while a pass was decided on balance.

### A command decides nothing before the journal has agreed to record it

Every session command validates the journal's next position first, without
mutating anything. Only then may an aggregate decide. An evaluation that
accepted a session boundary the journal then refused would leave the challenge
in one trading session and the log in another, with nothing able to reconcile
them.

For the same reason a command applies before it records: an order the account
refuses leaves no order, no fill and no position change in the log, no counter
moved and no money changed. Execution against one quote yields at most one fill
today; when a single order can produce several, or when a journal can fail on
I/O, this needs a real transaction boundary rather than the ordering of a few
lines.

### A log records one position per event, and names its cause

An event carries its own sequence, which is its place in the journal. A
challenge decision also has a cause — the input that produced it — and that is
recorded as `CausedBySequence`, not as a second nested field also called
`Sequence`. Two fields of the same name meaning different positions is a defect
waiting for a serialiser.

Counts in the log name what they count. `OrdersSubmittedThisSession` counts
orders, not trades and not fills: an order may not execute, may fill partially,
or may later be cancelled, and conflating the three misreports behaviour as
soon as any of those exist.

### Reconstruction restores a session, not only its aggregates

`Replay` rebuilds everything needed to carry on: the account, the evaluation,
the trading session's open state and identifier, the last book seen, whether
this session has been observed, and the behavioural counters a decision's
context is measured against. `Resume` continues from it, and the proof is that
a run cut in half and resumed produces the same stream as one that was never
interrupted.

### A trading day is stated by the data, not derived from a calendar

The canonical market file carries a `SessionID` on **every row**, not once at
the top. That costs a little size and buys two things: the boundary the domain
will consume is visible on the face of the file, so a misplaced row is
detectable immediately; and reading a file depends on no time zone, no daylight
saving rule, no holiday calendar and no version of an external calendar.

Converting a provider's raw data into that format is a separate
responsibility. A normalizer will need exactly those external rules, they
change, and it should carry its own decision record:

```text
provider data -> normalizer (calendar, time zone) -> canonical file -> adapter -> session
```

An adapter reads a stream it can vouch for; it does not decide what a day is.

### An observation belongs to a trading session

`Observe` refuses a quote when no trading session is open, before anything is
written, and `Replay` refuses a journal holding one as a structural fault. It is
one rule on both sides, and it has a second half the writer already kept:
`Observe` refuses market once the evaluation has ended, and `Replay` now refuses a
journal that records some.

The reason is the rule about waiting orders: every observation is offered to what
is waiting before the account is revalued, so the valuation an observation
records already contains what that observation caused. With no session open there
is nothing to offer a quote to, and accepting it anyway moved the last book past a
price no stop had seen. The next `OpenTradingSession` then valued the account
against that price — and could end the evaluation inside the batch that opened the
session, against a book the execution never saw. Nothing on either side caught
it, because the reader only asked survivors to explain themselves when it judged
something had been offered — and it judged that nothing was offered with no
session open, or after the evaluation ended.

That judgement was the two refused cases under another name. With both refused,
every observation the reader accepts was offered to what was waiting, and the
condition under which a survivor owes an explanation is simply that there is a
last book. The flag that carried the judgement is gone. A journal forged to add
market after a failure, walking through a stop still waiting, used to replay
clean; it is now a structural fault.

With the rule, an evaluation cannot end in the batch that opens a session, by
construction. Between the last observation of one session and the open of the
next nothing can change the book or the account: no quote is accepted, and fills
only happen when a quote is offered. So the valuation an open records is the last
one of the session before it, already evaluated against the static floor, the
trailing threshold and the target, and the daily reference is set to that same
equity, so the daily rule reads zero. The market that would have crossed a stop
between two sessions arrives inside the next one instead, and the stop is offered
it first.

No file-driven adapter ever produced the refused state — `Consumed` already
requires every observation to sit in the session the file assigns it — and no
frozen payload contains one.

### A failed commit stops the session for good

A command executes in memory and its events are then committed as one batch. If
that commit fails, the session is finished: every later command is refused, no
further mutation or write is attempted, and the failure is reported as
`ErrSessionNeedsRecovery`.

It is not healed in place. Recovery builds a **new** session from the confirmed
batches, and the original command is never re-executed automatically. That is
what answers the ambiguity: if recovery finds the batch, the command committed;
if it does not, the command did not. Re-running it would duplicate a decision,
and a duplicated decision is indistinguishable from one the trader really made
twice — which in a system built to measure behaviour corrupts the measurement
itself.

The aggregates mutate before the commit is attempted. That is deliberate: a
copy of the state would be a second representation whose only consumer is the
transaction, and a field forgotten in a copy function fails silently, whereas
rebuilding reuses `Replay`, which already proves what it reconstructs.

### Resuming verifies the market, it does not count it

A run continues a journal by matching every observation the journal holds
against the row in the same position of the market file — instrument, logical
time, source sequence, trading session, both sides of the book and their sizes
— and carries on from the first row that is not there. Skipping a count of rows
would replay a file that had changed as though it were the one that produced
the journal, and the resulting history would describe a market that never
happened.

That is why an observation records the source sequence it was given. Two rows
at the same instant differ only in that, so without it a file whose same-time
rows had been swapped would verify against a journal that did not describe it.

A journal's configuration comes from its own `SessionStarted` and never from a
flag: an account and an evaluation already have a history under the rules they
started with. The instrument is checked even when the journal holds no
observation to compare, because a run interrupted straight after committing its
configuration would otherwise resume against anything at all.

Running the same file twice appends nothing. A damaged tail is not repaired in
passing: the command says to run `praxis store repair`.

### Intact is not the same as true, and the commands say which

Inspection checks frames: lengths, checksums, continuity. A forged fact,
re-checksummed, passes it perfectly — the bytes really are the bytes that were
written. Checksums answer "were these damaged" and cannot answer "could this
have happened".

`praxis store verify` answers the second, by replaying the journal against the
account and evaluation that would have had to produce it. It exists as its own
command, with its own exit code, because an operator reading "clean" will
believe the stronger claim, and until it existed the defence ADR-012 names was
reachable only as a side effect of resuming a run.

### One projection, three readers

A position episode is derived by a single projection used by the live session,
by `Verify` and by `Replay`. A second implementation would eventually disagree
with the first, and the disagreement would be between a journal and the thing
that checks it.

The distinction between the two readers survives it. In `Verify` the projection
shows the log is consistent with itself; in `Replay` it runs on position
changes already proved against what applying the fill produced, so what it
derives rests on facts rather than on the log's word for them.

A decision carries the streak that was true when it was taken. The order that
flips a position therefore records the count from before the flip — the episode
had not ended when the trader decided — and the next order sees the episode the
flip closed. Attributing the later figure to the earlier decision would credit
the trader with knowledge they did not have.

### A triggered stop cannot untrigger

A stop that reaches its level has become a market order, and a market order does
not wait. What the book cannot fill at that moment is cancelled and recorded as
an unfillable remainder, exactly as for a market order — never left working,
because a later observation above the level would untrigger it and the trader
would be protected by an instruction the market had already passed.

This is why execution reports a result rather than fills alone: no slice of
fills can tell a stop that was never reached from one that was reached with
nothing to trade against, and only the second is irreversible.

### A journal is not believed, it is proved

A log is not a source of truth because it is well formed. Every derived fact in
it is rebuilt during replay from the facts that caused it, and compared
exactly:

- an account valuation against the account and the last book, marked by the
  same rule a live session marks with;
- a position change against what applying that fill actually produced;
- a challenge decision, and the input it names as its cause, against what the
  evaluation actually decided.

Replay also refuses a log that does not describe a session: an event whose kind
contradicts its type, time running backwards, a non-contiguous sequence, or a
trading session opened, valued or ended out of turn.

**A decision carries two clocks, and only one of them is held to an order.**
Neither can be derived — both are adapter data, like an observation's source
sequence — but both can be coherent or not, and both readers ask.

`AtUTC` says when in the world something happened and is **never compared for
order**. A wall clock moves backwards legitimately: a time server corrects it,
an operator sets it, a suspended machine resumes. An earlier draft of this
refused a backwards wall clock as a corrupt journal, which would have refused a
session that was entirely honest — one connection to one kernel does not make a
wall clock monotonic, and the failure it was catching was a real one solved in
the wrong place.

`Segment` and `ElapsedNanos` are where intervals come from. A segment is a run
of uninterrupted interaction, numbered from one; a recovery or a reload starts a
new one, because the monotonic reading that made `ElapsedNanos` meaningful did
not survive the interruption. Within a segment it never goes back; a new segment
may begin at any value; **an old segment never reappears**, or a stale tab could
interleave its decisions with a resumed session's; and **an interval is never
computed across two**. A wait that spanned a recovery is recorded as having
spanned one, and whether such cases are excluded is a question for the pilots
rather than for the engine.

The units are in the names — `UnixNanos`, `ElapsedNanos` — because a journal is
read by people who did not write it, and an integer timestamp whose unit has to
be inferred is a format that means two things.

`GestureID` names the act rather than the thing acted on, and that is what makes
**every** human command idempotent rather than only a submission. An order
identifier covers a resent order; it does nothing for a resent replacement, a
resent withdrawal or a resent cancellation, each of which would otherwise arrive
as a second human decision — and in a log that exists to hold decisions, an
extra one is not a duplicate record but a falsified finding. A gesture is spent
forever, exactly as an order identifier is, and the set is reconstructed from
the journal rather than held in a server's memory, so it survives a restart.

A decision is wholly present or wholly absent: a stamp with a moment in the
world and no segment would read as an absence while plainly recording that
somebody acted. And it agrees with the subject — a journal somebody traded
records a decision for every human command in it, one nobody traded records
none, and an event the log itself required records none either, because nobody
commanded it.

Latency is `decision.ElapsedNanos − presentation.ElapsedNanos` **within one
segment and from one clock**. If the presentation were stamped by the server and
the decision by the browser, the subtraction would mean nothing. For a local
interface both come from the server — the moment the acknowledgement arrived,
and the moment the command arrived — which includes render and transport in the
measure, but both are small and consistent. If the pilots show that separating
rendering matters, the browser acknowledges presentation and both stamps move
there, together.

**The client sends no clock at all**, and that is stronger than sending one that
must agree. The lease records when its segment began, and every request the
server admits is stamped on arrival against it: the wall reading for audit, and
an elapsed measured from the segment's start with Go's monotonic reading, so a
clock correction cannot move it. A browser figure would be the one quantity in
this journal that nothing can contradict — monotonicity catches a reading that
goes backwards, and catches nothing about one that runs slow, fast or invented —
and this system recomputes derived facts rather than believing them everywhere
else.

The clock hands back two readings and not one. A wall time for audit, which may
legitimately move backwards, and a monotonic count which may not — the two facts
an `Instant` already models apart. A clock that returned one `time.Time` would
collapse them and leave the receiver to split them again, which is how a wall
reading ends up inside a measurement by accident. It is not hypothetical: an
elapsed computed on the wall would go backwards after a correction, `checkOrder`
would refuse the command, and `Apply` runs only after a successful record — so
every act for the length of the correction is refused with "your reading went
backwards", to a participant who did nothing, and then recovers on its own with
nobody able to say why.

The clock is injected rather than called. Rule 2 forbids one in the domain and
allows one in an adapter; injecting it is what makes a run reproducible, and
that is not a convenience. It is what allows the same acts taken through HTTP
and taken directly to be compared byte for byte, which is the only way "the
interface is an adapter" is a fact rather than an intention.

A consequence worth recording, in the same family as `ErrGestureReused` becoming
unreachable over HTTP: with the server stamping, the chronology rules cannot be
broken by a client. `ErrInteractionOrder` is unreachable through the interface,
because the readings the interface produces are monotonic by construction. The
kernel's guard stays, for a direct caller — which is what every test in the
suite is.

`ObservationPresented` is that other end, and **its name is the whole of its
claim**: the browser finished rendering and said so. It does not mean the
participant looked at the screen, and nothing may be read as saying they did.
Recording it when the response is written would be worse — that says only that
the server tried to send something, and the connection may have dropped with
nothing drawn. So the interface acknowledges, and the acknowledgement is what is
stamped.

A presentation is identified by `<segment>:<observation sequence>`, derived
rather than minted. The same quote shown again after a reload is a *different*
presentation, because a reload is a new run of interaction and the reading an
interval is computed from starts over. From that follow the rules the readers
enforce:

```text
a repeated acknowledgement records nothing      a retry, not a second showing
one naming another observation is refused       a stale tab must not stand in
one made in another segment is refused          the interval would be on another clock
a human command before it is refused            a decision with no beginning
each observation is confirmed on its own        a new quote is a new showing
a resumed session keeps its confirmation        reconstruction is faithful
a new segment does not inherit one              it has seen nothing yet
```

Checking a log against itself is not enough, and the difference is testable. A
forged realised amount can be made internally coherent by adjusting every
context after it; `Verify` accepts that log and `Replay` refuses it, because
the account that produced the fill never realised that amount.

**Everything is proved relative to the configuration, and the configuration is
not proved at all.** The commission a fill was charged, the balance it started
from, the rules it is judged against and who traded it are axioms. A session run
at zero commission loses five hundred cents of fees and is entirely
self-consistent afterwards, because the fills that fed the account were charged
what the configuration said and every valuation compares the account with
itself. No check inside a journal can catch it, and adding one would only move
the axiom.

That is not a defect to close. It is what a pre-registration is for: the
configuration is fixed before any session is traded, and the journal answers for
being consistent with it, not for justifying it. So `praxis store verify` prints
a digest of the event that opens the journal —

```text
config:    sha256:9f2c…
subject:   t-01
```

— which is what a protocol records beforehand and what anyone can confirm
afterwards. With a subject inside the configuration, *which trader produced this
journal* became an unproven claim living inside the structure whose purpose is
that claims are proved; this is where it is answered, and it is answered from
outside.

**Neither reader dominates the other, and a caller needs both.** `Replay`
proves what the aggregates produced. `Verify` proves the derived fields a
decision carries — the context on an order, whether a stop widened, the levels
a protection was holding when it ended — none of which `Replay` recomputes.
`praxis replay` and `praxis store repair` run both, in that order, and that is
a requirement rather than a coincidence. Where their obligations overlap,
`Verify`'s are a prefix of `Replay`'s: `Verify` cannot see whether a fill
reversed a position, so it demands the same consequences with the end reason
unchecked, and never a different one.

**What did not happen is proved too.** Everything that filled is proved against
the account. The *absence* of a fill was, for several slices, believed — and it
is the only forgery direction that flatters a trader, because a journal in
which a stop the market traded through is still waiting shows a loss that never
happened, against a flat account no valuation contradicts.

Every transition an order can make is now re-executed against the book **as it
was left** rather than as it arrived:

- a **fill** must be exactly what the policy would have produced — the same
  price, the same quantity, from an order that was actually submitted or a
  protective leg that actually exists;
- a protective leg cancelled for want of liquidity must have reached its level
  and found nothing;
- an order's remainder cancelled as unfillable must belong to an order that can
  leave one — a market order, or a stop that triggered — must match the fills
  the journal itself records, and must meet a book with nothing left on it;
- **everything still waiting when an observation ends must be unfillable**, and
  no stop among them may be triggered;
- and **every order reaches an end**: it fills, it is cancelled, or it is still
  waiting when the log stops. One that reaches none is otherwise invisible —
  it never enters the working set and it moves no money.

A fill's price is where the conservatism rule is easiest to break with no
trace. A market buy recorded ten ticks below the ask is ten ticks of free
improvement, and every later check agrees with it, because the fill is what fed
the account and a valuation compares the account with itself. Subtracting a
fill from the book can also no longer drive it negative: execution stops at the
size the observation displayed, so only a journal can produce one, and a
negative book is the arithmetic saying a fill took depth that was never there.

Judging against the consumed book is what makes the last one strict without a
second copy of the resolution order. Everything that filled has already been
taken out of the book, so what remains is exactly what the survivors were
offered: a limit whose price the market reached but whose depth an order in
front of it took has nothing to answer for, and a stop at the same level does,
because triggering owes nothing to liquidity. Nothing is asked of an order no
trading session was open to offer anything to, which is the same gate the live
session applies.

**One thing is still believed: the interleaving.** When two orders competed for
depth the observation did not have enough of, nothing recomputes which of them
was offered it first — the journal's own ordering decides, and each fill is
then checked against the book that ordering implies. A log that gave the scarce
contracts to whichever order suited the trader is internally consistent. Every
other direction is closed:

| the order… | proved by |
|---|---|
| fills more than the book showed | the fill's re-execution |
| fills at a better price | the fill's re-execution |
| fills from nothing | the fill's re-execution |
| does not fill and rests | the survivor check |
| does not fill and is cancelled | the remainder's re-execution |
| does not fill and disappears | every order reaches an end |
| fills in part, the rest rests | the survivor check |
| fills in part, the rest is cancelled | the remainder's re-execution |
| fills in part, the rest disappears | every order reaches an end |

Closing the interleaving means running the whole resolution order inside the
reader. `foldPositionChange` is the first half of that, and it is a narrow case
that matters only where two orders actually competed.

Verification uses checked arithmetic throughout, and a live session uses the
same arithmetic on the same counters. A verifier that silently wrapped would
accept a corrupt log for exactly the reason it exists to reject one, and two
implementations of one increment would eventually disagree.

### A position is marked at the price it could be closed at

A long is valued at the bid and a short at the ask, never at a midpoint and
never at the side it was entered on. The exit side is what the position would
actually fetch; anything else shows money that could not be realised. The
session layer applies this, because it is the only place that knows both a
position's direction and the current book.

A consequence worth naming: a position is worth a spread less than it cost the
instant it is opened, and that shows in equity immediately. That is correct,
and it is the same conservatism as execution crossing the spread.

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

### Loss rules precede the profit target, and the daily limit precedes the floor

Both loss rules are evaluated before the target, and the daily limit before the
static floor. When the daily limit and the floor breach together the outcome is
identical either way, so the ordering exists only to keep a recorded reason
stable rather than incidental.

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
    // Returns a Result and not fills alone, because an empty slice means two
    // different things: a stop whose level was never reached, and a stop that
    // reached it and found nothing to trade against. The first may wait; the
    // second has already become a market order and cannot untrigger.
    ExecuteOnQuote(o Order, q Quote) (Result, error)
}

type IntrabarResolutionPolicy interface {
    // Reports an error for input that cannot describe a resolution, as
    // ExecutionPolicy does. Bar, Position and ProtectiveLevels all have
    // exported fields, so a consumer cannot assume a constructor was used.
    Resolve(bar Bar, pos Position, lv ProtectiveLevels) (IntrabarResult, error)
}

// A session knows only that one command produced one ordered group of events.
// Commit means the whole batch is durably confirmed; any error means the
// outcome is unknown until recovery examines the store, so it must not be
// retried. Batch numbers, checksums, paths and durability policy belong to the
// store and never cross this boundary. See ADR-012.
type BatchCommitter interface {
    Commit(events []Event) error
}

// Reserved. Nothing in Praxis is random yet, so nothing implements this and
// SessionStarted records no seed: an unread Seed: 0 would be less truthful
// than its absence. When randomness first enters, an injected Randomizer and
// an explicit recorded seed become required together.
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

Complete. Conservative quote execution, worst-case intrabar resolution,
determinism, and property tests are implemented.

### Phase 1 — Position and Account

Complete. Exact cost basis, opening and adding both sides, partial and full
closes, flips, realised and unrealised P&L, accumulated commissions, balance and
equity are implemented with checked arithmetic and deterministic tests.

### Phase 2 — Challenge engine

Implement `Pending -> Active -> Passed | Failed` with invalid transitions
unrepresentable. Rules are configuration. Cover static and trailing drawdown,
unrealised breaches, high-water marks, session-boundary resets, positions across
boundaries, time zones, contract limits, minimum days, and consistency rules.

### Phase 3 — Behavioural event log ✅ Done

Append-only persistence and a minimal replay CLI. A full session reconstructs
from its journal without information loss, and every derived fact in it is
proved against the aggregates that produced it. See ADR-012 and section 10.

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

Phases 0 and 1 are complete and hardened. Phase 2 now applies four equity
rules: daily loss against a session reference, a static floor anchored to the
configured starting balance, a profit target on realised balance, and trailing
drawdown on total equity. The state machine is terminal in both directions.

The trailing high-water mark includes unrealised P&L, never falls or resets at
a session boundary, and does not freeze before the evaluation ends. Within one
snapshot the engine validates first, computes the candidate high-water mark,
derives its threshold, evaluates the same equity, and commits atomically. A
zero configured amount disables static or trailing drawdown, but zero is also a
valid floor, so public accessors return an explicit enabled boolean rather than
using zero as an absence sentinel. Failure precedence is daily loss, static
drawdown, trailing drawdown, then profit target.

The first orchestration slice is complete. Scripted commands drive execution,
fills reach the account, one atomic valuation reaches the evaluation, and every
fact is appended to a journal that reconstructs both the account and the
evaluation with nothing lost.

That journal records decisions, not only outcomes. `OrderSubmitted` carries the
state the decision was taken in—balance, equity, orders so far this session,
consecutive losing closes, session realised P&L, and the position it was
submitted into. Every one is a fact the system knows at that instant; none is
an interpretation, and naming a pattern is analytics that belongs nowhere near
this package. Because those figures are also derivable from the events before
them, `Verify` proves the log does not hold two contradictory truths.

Praxis now has a full operational path: a market file, a deterministic session,
durable batches, a crash, an explicit repair, and a resumed run that produces
byte for byte the journal an uninterrupted one would have.

Next, in order:

1. ~~A power sensitivity table.~~ Done:
   [`docs/experiment/power-sensitivity.md`](experiment/power-sensitivity.md).
   Its answer changes the plan. An effect measured in R is out of reach at any
   realistic sample, so the primary hypothesis is more likely to be a frequency
   — chosen because it represents the thesis, never because it is affordable —
   with effect-size questions kept exploratory and reported with intervals. The
   split-half rule costs more than doubling: two halves at 80% each give about
   64% jointly, and 80% jointly needs 2.63 times the single-sample figure. Which
   of the two the protocol means must be said before it is frozen.

2. ~~Decide the exact comparison the primary hypothesis makes.~~ Done:
   [`docs/experiment/hypotheses-candidates.md`](experiment/hypotheses-candidates.md).
   Three candidates, with numerator, denominator and exclusions; the
   provisional primary is the rate at which a stop is widened among trades
   opened after two consecutive losing closes, compared against trades that
   were not. It is a comparison of two observed proportions, so its power uses
   that shape and not the cheaper one-sample form. All three candidates need a
   trade identity, which is not modelled.
3. ~~Define a trade's identity and its ending.~~ Done:
   [`docs/experiment/trade-identity.md`](experiment/trade-identity.md). A trade
   is not one thing. An **episode** is flat-to-flat and is where money lives,
   because it is the only span over which weighted average cost gives an
   unambiguous P&L. An **entry** is one exposure-increasing decision and is
   where behaviour lives; it has no P&L of its own, and asking for one is what
   leads to FIFO lot tracking and a second accounting contradicting section 4.
   Protective levels attach to a decision, not to inventory. Both units are
   already delimited in the journal — episodes by `PositionChanged` kinds
   `opened` and `closed`, entries by `Order.ID` — so the only thing missing is
   the reference from a level to the entry that placed it. Three protection
   events suffice; no relation table is needed.
4. ~~Resting orders, and keeping a partially filled remainder.~~ Done. An
   order that the observation it was submitted on could not fill now waits for
   a later one, and what the book could not fill is named rather than dropped:
   a limit or a stop rests, a market order's remainder is cancelled and
   recorded, because resting a market order would invent a price the trader
   never named. One observation shows a finite book and everything executing
   against it consumes what it takes, so the same contracts are never handed to
   two orders. Waiting orders are offered an observation in the order they were
   submitted, and they are offered it before the account is revalued, so the
   valuation an observation records already contains what that observation
   caused.
5. **The life of a protective level**, done. Levels are planned with an entry
   in one batch, moved and withdrawn while it waits, ended with it if it is
   cancelled, activated by what a fill actually did, and executed
   one-cancels-the-other against the observation that activated them, with
   partial quantities. `praxis.event.v3` is published by the commands that
   place, change and withdraw; `praxis.event.v4` by the execution that needed
   cancellation reasons no earlier version has a name for.
6. **The eighteen transition scenarios** listed in ADR-014 all pass, and every
   protective transition is proved against the market rather than believed.
   One rule has no test and says so: with a two-sided quote a protection's two
   levels cannot both be reachable, so "the stop wins" waits for bar
   observations. A property test pins the impossibility in the meantime.
7. **A minimal local UI**, under the protocol constraints in section 11.
   Nothing else until the pilots are traded.
8. **Fifteen labelled pilot sessions — five traders, three each** — excluded
   from the confirmatory sample and used only to estimate the inputs the
   sensitivity table left open, including the between-trader correlation that
   sessions from one person cannot produce.
9. **Freeze the experiment**: the primary hypothesis, the minimum relevant
   effect, the analysis method, the power target, the sample size, the
   exclusion rules and the stopping rule.
10. **Only the challenge rules the frozen protocol requires.**

## 11. The interface is experimental apparatus

A screen looks like a layout decision and is not. Four of its choices change
what the journal means, and each is cheaper to settle now than after a batch of
pilots is invalid.

**One command replaces a protection.** Never a remove button beside a set
button. Two commands manufacture an interval with no protection the trader
never intended, and the length of those intervals is one of the things the log
exists to measure. `ReplaceProtection` is the command; the screen must not be
able to express anything else.

**Confirmatory sessions run at a fixed automatic cadence.** Every subject trades
the same file so that the market stops being a covariate and becomes a constant.
That holds for the prices and stops holding the moment pacing varies: two
subjects who moved through the same file at different speeds are not in the same
experiment, and the variance between them is confounded again.

An earlier draft of this said "advance one observation at a time, with no pause"
and that is a contradiction. If the participant decides when to press Next they
can stop for forty seconds before pressing it, which is a pause whether or not a
button is named one. Removing the button removes the name, not the behaviour. So
the confirmatory condition is: **a fixed automatic cadence, no pause, no rewind,
no speed control, and the same information visible to everyone.**

**The pilots advance by hand, and the gap between two presentations is the
record.** An earlier draft of this required the pilots to record pause,
resumption, speed and the moment each observation was presented. Only the last
exists, and the other three are not missing: with manual advance there is no
speed, and a participant who sits for forty seconds before asking for the next
observation produces that interval without anything being called a pause.

A button would record something a gap cannot — the intention to stop — and that
is exactly the substitution this document refuses everywhere else. `OrderContext`
records facts and leaves naming a pattern to analysis; a pause button is an
interpretation the participant supplies, and the gap is how long they actually
took. Pause is also barred from confirmatory sessions by the paragraph above, so
its events could only ever appear in journals the confirmatory sample excludes:
the weakest possible case for a payload version.

So it is not built, and the cost is stated rather than discovered: if the pilots
run without a pause control and it turns out one was needed, those sessions are
not re-run. Asking the five participants whether they wanted to stop belongs in
the pilot protocol, before the first session and not after.

**Which of the two a session was is configuration, not a screen setting.**
`Config.Pacing` is `scripted`, `pilot` or `confirmatory`, so it sits inside the
digest a pre-registration records. A participant who could change the pacing
could change the experiment, and a journal that did not say which condition
produced it would be a session nobody can classify afterwards.

The pacing and the subject are **one claim**, and both readers refuse a journal
that makes it twice:

```text
scripted             nobody named, and no decision anywhere
pilot, confirmatory  somebody named, and every human command carries a decision
```

A test fixture that needs a driven session is `scripted` and anonymous. It must
never be a journal labelled as though a person had traded it, because that is
the one thing the pilot sample cannot contain.

The confirmatory mode is **not built yet**, and should not be until the pilot
interface has shown it can measure presentation, gesture, recovery and exclusive
control without losing or duplicating a decision.

**What is on screen is a claim the journal is already making.**
`OrderContext.ConsecutiveLosingTrades` is documented as what the trader knew,
and the only thing that can make that true is the interface showing it. If the
streak is not on screen the field is still a fact about the world, but the
comment is false. Worse, showing the distance to a trailing drawdown threshold
changes what a hypothesis about rule breaches on losing days is measuring:
"people break rules when already down" becomes "people react to a number they
were shown". The inventory of what is displayed is written before the screen is
drawn and belongs to the protocol, not the layout. It is this:

| field | what it is |
|---|---|
| `subject`, `pacing` | which run this is, and under which condition |
| `cursor`, `observations` | how far through the file, and how long it is |
| `observedSequence` | which observation is on the screen, so a confirmation can name it |
| `sessionOpen`, `sessionId` | whether a trading session is open, and which |
| `book.time`, `book.bid`, `book.ask`, `book.bidSize`, `book.askSize` | the book as the session holds it — what is left, not what the file offered |
| `position.symbol`, `position.netQty` | what is held |
| `money.balanceCts`, `money.equityCts` | settled and marked, by the session's own valuation |
| `evaluation.state`, `evaluation.reason` | active, passed or failed, and why |
| `consecutiveLosingTrades` | the streak `OrderContext` will record on the next decision |
| `working[]` | id, side, type, qty, limit price, stop price |
| `protection[]` | status, entry order id, episode id, stop price, target price, protected qty |
| `needsRecovery` | the session has stopped, or its valuation could not be taken |

Nothing else. In particular: no distance to any threshold, no high-water mark,
no drawdown figure, and no count of anything the participant has not done.

**`observedSequence` is the journal position, and that is a decision rather than
what was to hand.** An opaque token would read as nothing, but it would need a
mapping the server keeps and a restart rebuilds — machinery bought with no
measured gain. The raw sequence is safe to show for a reason that can stop being
true: every event the journal records is either on this list or derivable from
something on it, so the gap between two sequences tells a participant nothing
they were not already told. If the journal ever records something the screen
does not show, this field becomes a channel and has to be reconsidered.

The test that enforces this list cites it. It used to *be* it — the only
enumeration of what a participant may know lived in a `_test.go`, so "the
protocol approved it" was unfalsifiable and a pre-registration would have been
citing a file that the same commit adding a field can edit.

**A name the record cannot hold is refused at the door — and the rule is the
format's.** Identifiers are letters, digits and `. _ : -`, and the domain
enforces that for an order, a trading session boundary, a subject and an
instrument symbol alike.

This is **the file format's alphabet adopted as a domain rule**, and it should
be read as exactly that rather than dressed up as a property of instruments.
Saying "MN Q is not a bad instrument, it is a journal that cannot begin" argues
the opposite of the conclusion: the limitation belongs to the format, and the
domain takes it because the alternative is worse — a name the kernel accepted
and the journal could not write was a valid command that poisoned the session at
commit time, three good batches in, refusing every correct order after it. A
system whose purpose is the record cannot let a decision exist the record has no
way to contain.

The restriction is acceptable for MNQ and the pilots. It is **not** a claim
about what a tradable instrument may be called: a real symbol set with spreads
or exchange-specific punctuation would need the format to learn an encoding, not
the domain to declare the instrument invalid. Whoever meets that first should
change the format, and this paragraph is here so that they know the domain rule
is downstream of it.

The codec keeps its own gate, because a decoder must not trust the bytes it
reads, and a test holds the two to the same set rune by rune rather than
assuming they agree.

**The order identifier comes from the gesture, not the server, and it is
deterministic.** A duplicated submission — a double click, a retry after a
timeout, a second tab — is the one way an interface puts a decision in the
journal that no person took. The client mints the identifier at the instant of
the gesture and sends it, so a retry sends the same one and the kernel refuses
it. Identifiers are already spent forever and never reused, so a whole class of
interface bug becomes a domain guard that exists and is tested.

It is `<Segment>:<GestureSequence>`, and the segment is **issued by the server
with the lease**, not minted by the client.

An earlier draft asked the client for a `<SubjectID>:<RunID>:<GestureSequence>`
with the counter persisted before the request went out. That is not
constructible. Rule 3 forbids a random identifier — a browser's random source is
a value nothing can reproduce, deciding which of two journals is the real one —
and a counter alone does not survive a reload. Nothing a browser can see
guarantees uniqueness across restarts, because the only thing that does is the
journal, and the client cannot read it.

The segment already has exactly that property and is already proved to have it:
`newLease` starts from the highest segment the journal holds, so a reload takes
a new lease and is issued a higher one. The counter can then live in memory and
start at one, and the persisting the old draft required is a mechanism that
would now do nothing. The subject is not repeated in the identifier: it is in
the configuration, constant for the whole journal, and a third copy has no
reader.

The segment therefore appears twice — in the identifier and on the decision —
and that is a duplication, so it is checked rather than trusted: `checkDecision`
refuses an act whose identifier names a segment its decision does not. Same
discipline as `Widened`, and the refusal names the actual fault instead of
reporting a confusing "this gesture is already spent".

**And it makes a failure the segment rule cannot prevent into a refusal.** Two
tabs sharing one lease can interleave their gestures inside a single segment
without breaking any rule above; with the client minting `<Segment>:<Sequence>`
they collide on the sequence, and the second is refused as an act already
recorded. That turns a fault which would have duplicated a decision into one
that rejects it. It is written down because a behaviour nobody wrote down is one
the next change removes without anything going red.

The identifier travels on the decision, not on the order, so it covers every
human command and not only a submission.

**A set of spent names is not enough to answer a retry.** It says the act
happened; it does not say what it did, so it cannot tell a resend from a
different command sent under a name reused by mistake. So the journal yields
what each act *commanded*, reconstructed by `Replay` and carried by `Resume`:

| the act | what a retry is compared against |
|---|---|
| submit an order | the whole order, and the stop and target placed with it |
| cancel an order | the order it named |
| replace a protection | the reference and the new levels |
| withdraw a protection | the reference |

The stamp is deliberately **not** part of that comparison. The server is the
clock, so a retry arrives at a different instant; comparing stamps would make
every retry a conflict, and re-stamping one would record a decision at a moment
the person decided nothing. The original `Decision` is recovered from the
journal.

That gives the loop three answers and only one of them touches the kernel:

```text
a gesture never seen             -> add the time, execute, commit, respond
the same gesture, same command   -> already committed, with the current state;
                                    no kernel call and no new bytes
the same gesture, other command  -> conflict
```

A repeated request that already committed must not come back as "that name is
taken". A lost HTTP response is a retry, not a false alarm in front of the
participant. The client's counter prevents accidental reuse within a run and the
segment keeps two runs apart — neither proves confirmation. **The journal is the
authority**, which is why the index is rebuilt from it and survives a restart.

**One lease on the controls, not one TCP connection.** A connection is the wrong
unit: it drops and returns for reasons that have nothing to do with who is
driving.

```text
one active lease at a time
every command presents its lease
taking a new lease opens a higher segment
an old lease is never valid again
a second tab is refused: 409, someone else is controlling
losing the view may reconnect on the same lease
handing control over is explicit, needs the operator's handover key, and opens a new segment
```

The lease token is infrastructure and never enters the journal. `Segment` does,
because it changes how the times are read.

**The segment rule does not prove the lease held**, and an earlier draft of this
claimed it did. It proves the journal keeps a coherent chronology of segments —
two tabs sharing one lease could interleave their gestures inside a single
segment without breaking it at all. Exclusive control is an invariant of the
adapter, and it needs its own tests: concurrent acquisition, a stale token
refused after a transfer, and a late request from a lease that has been handed
over.

One active connection: a second tab is a second hand on the wheel, and the
kernel's inputs must arrive in one order.

**Taking the controls from their holder needs a key only the operator's console
shows, and it works once.** There is no way to give a lease up, so a closed tab
keeps its lease and the next one is refused; the transfer exists for exactly that
case, which is why it cannot ask for the current lease — its holder is the one
that is gone. Without anything to present, any process on the machine could take
a participant's controls, and its decisions would enter their journal as theirs:
the one failure of the adapter that contaminates the record rather than its
availability. So `praxis ui` prints a handover key when it starts, a transfer must
present it, and the transfer spends it and prints the next one. The key is checked,
the controls granted and the key replaced under one lock, so two transfers racing
on one key grant one. Like the lease token it is infrastructure randomness and
never enters a journal. A server given no way to show the key still mints one,
which fails closed: controls nobody holds can be taken, and controls somebody holds
cannot.

Three smaller decisions, settled the same way:

- **A recovery is a measurement.** `Recover` → `Resume` must be invisible to
  the trader, and something must still count it: sessions lost to interruption
  is one of the six quantities the pilots exist to estimate.
- **A `ReplayedState` is consumed by one `Resume`.** Two share one account and
  one evaluation, which fails loudly rather than silently — but an interface
  that retries a recovery can reach it.
- **Quantities cross as canonical integers, parsed totally and formatted in
  Go.** Nothing crosses with a decimal point: every figure is a whole count of
  its smallest unit, and the field names say so — `balanceCts`, not `balance`.
  A spelling that is not canonical is refused, never repaired: no leading zero,
  no sign on a positive, no negative zero, no empty string. `market.ParseInt`
  and `market.FormatInt` are that rule and are exact inverses, and the journal,
  the market file and the interface all use them. The rule lives in the domain
  because it is how a `Cents`, a `Ticks` and a `Qty` are written down and none
  of the three boundaries owns the spelling — and because a second reader, each
  deciding for itself whether `020000` is a number, is a second policy. No
  `toFixed` in JavaScript either, for the same reason.

**A refused attempt leaves no trace, deliberately.** `prepareOrder` refuses
before recording anything, so an order with inverted levels never reaches the
journal. If attempting a widening counts as behaviour — and an attempt to widen
with levels the geometry rule refuses *is* an attempt to widen — it belongs in
an interaction log the interface keeps, never in the journal. Which of the two
it is has to be decided, because the frozen protocol will have to say.

### The shape of a command, decided before the first handler

Seven decisions, written here rather than settled by whichever handler was
written first. They are protocol: a client author reads them, and changing one
after a pilot has run means the pilot ran under a different protocol.

**One route, a tagged body.** `POST /api/command`, and the body says which of
the four human commands it is. Not four routes, because the preamble every one
of them needs — find the gesture, check the lease, compare, execute — would be
copied four times and the four copies would drift, which is the defect this
document has spent its last several revisions removing. A tagged payload is a
shape this system already knows: it is what an event is. It also puts the
canonical integer parsing in one decoder instead of four, which is where
`market.ParseInt` earns its place at this boundary.

**Finding, checking and executing happen inside one turn of the loop.** All of
it in a single `ask`. Two turns would let two concurrent retries of one gesture
both find nothing and both execute. The lease cannot serialise this — the
paragraph above says a segment does not prove exclusive control, and two tabs
sharing a token can interleave — so the loop is the only serialisation there is,
and the check and the act have to be on the same side of it.

The lease check goes inside that turn too, and for the message rather than the
race. A transfer between an outside check and an inside execution leaves a
command running under a revoked lease; the clock catches it, because the old
client's segment is now behind, but it reports *your reading went backwards* to
someone whose controls were taken away. The refusal has to name what happened.

**A retry answers 200 with the current state, indistinguishable from success.**
A lost response is a retry, not a false alarm in front of a participant. Current
state means *now*, not the state the original command produced: if three
observations arrived in between, the retry carries those. That is correct — the
client paints what it receives — but it means the response cannot be read as
"this is what my command did". It does not need to be: the effect is in the
position, the money and the orders standing. The server counts retries, because
§8 measures sessions lost to interruption and a retry rate is a fact about the
network; the participant is shown nothing.

**A success and a retry return the same body: the whole `State`.** The client
never has to chain a GET, and the two paths returning one shape is what makes
them indistinguishable in fact rather than only in status code.

**The status is a coarse bucket and the reason is typed in the body.** There are
several distinct 409s already — a lease held by someone else, a stale lease, a
gesture reused with a different command — so a client discriminating on status
cannot. Every non-200 carries a typed reason from a closed set enumerated here,
because a reason that is not enumerated is a string a client will match on and a
string will drift. That is the `strings.Contains` problem moved one layer out,
and it is the same fix as `%w` one layer in.

The scope is every answer of the four command routes, the state route and the
guard in front of them. The one exception is the 404 a request for a route that
does not exist gets, which comes from the mux and not from this server.

| status | what it means | reasons |
|---|---|---|
| 400 | the bytes are wrong | unreadable body, non-canonical integer, invalid identifier, unknown tag |
| 403 | the request is not one this server answers | an origin that is not this server's, a transfer without the current handover key |
| 405 | the route is asked for in a way it is never asked for | a command that is not a POST |
| 409 | something is already taken | lease held, lease stale, same gesture with a different command, an act naming a run it does not belong to, a step from a row no longer on the screen |
| 422 | the session refuses this command now | reading out of order, nothing presented, evaluation ended, no session open, no row after the last one |
| 500 | the machine could not do something that cannot fail on purpose | a lease that could not be minted |
| 503 | the session has stopped, or this process is | needs recovery, and the screen says so; or the server is closing |

**A server shutting down and a session needing recovery are two findings, and
they had one name.** A request that does not reach the loop because the process
is closing was answered `needs_recovery`, which sends an operator to inspect a
journal with nothing wrong with it. It is `server_closing`, and `needs_recovery`
is left to the session that actually stopped.

**Every reason must be producible, and a test proves it rather than a reader.**
`lease_held` was declared from the first day and unreachable until the one place
that could emit it stopped answering in plain text. So a test reads the constants
out of the source, and demands that each one is produced — by the classifier or
by a handler answering a real request — or listed as unreachable with the reason
it is. A reason declared and never connected fails immediately.

**The retry is recognised in the handler, so `ErrGestureReused` becomes
unreachable over HTTP.** `checkDecision` refuses a repeated gesture without
looking at what it committed; the handler looks first, and answers a matching
command with the state. The kernel's guard stays: it is the guard for a direct
caller, which is what every test in the suite is. It has a test of its own —
the same gesture twice must not reach the kernel — so the interception is a
decision and not an accident of the order two lines are written in.

**The acknowledgement and the step are idempotent by content, and the four
commands by name.** Neither carries a gesture. The acknowledgement's identity
*is* its content — the segment and the observation — so there is no "same name,
different command" case for it to have, which is why it needs no name. Repeating
it records nothing and answers 200, and it answers before the chronology is
consulted, deliberately: a retry carries the reading it originally sent, which
the log has legitimately moved past by then. Two forms of idempotency in one API
is a hazard, so a client author is told which is which here rather than
discovering it.

**A step names the row it advances from, and that is its whole identity.** It
is not a decision the journal records as one: the row it produces is recorded,
and the time a person took before asking for the next is the gap between two
presentations. So it has no gesture, and a lost response is the case it has to
survive — applied twice, a retry would put a row in the journal that nobody saw
and the journal would not say one was skipped. The body carries
`fromObservedSequence`, the observation the state named when the person asked;
absent is the empty string, for a session that has shown nothing, and zero is
refused as a second spelling of that. The order of its checks is a decision:

1. A step from a row that is not on the screen is a **retry** if the last step
   in the same run started from that row and nothing has moved since, and is
   answered with the state. Otherwise it is **stale** and refused. A retry means
   something only inside the lease that sent it; a restart grants a new one, and
   a request from the old one is refused before this is asked.
2. The row on the screen must have been **confirmed in this run** before the
   market moves past it. A row that passed with no presentation would be a hole
   in the one quantity the pilots record. A session that has shown nothing has
   nothing to confirm.
3. Only then is a row applied, and the file running out or the evaluation
   having ended are refused before anything is written.

The retry is answered before the confirmation is asked for, the order the
acknowledgement answers its own retry in: a lost response leaves a row the
client never saw and so never confirmed. The row itself is applied by
`marketdata.Step`, the same function `Drive` loops over, so a run advanced by
hand and a run driven to the end cannot apply a row differently.

**A restart continues above every run the chronology holds, not every run that
issued a command.** With manual advance, a run in which a person only looked
and confirmed is the ordinary run. Counting commands to find the highest segment
granted that run's number again, and its first confirmation was refused as out
of order — a journal nobody could continue. The number comes from the
interaction clock, which every stamped class already advances.

### One chronology, five stamped events, three classes

Everything a person did to a journal belongs to one non-decreasing chronology,
and one machine holds it — `interactionClock`, asked by the live session, by
`Verify` and by `Replay`. It was two rules in two places before, and the writing
side ran the smaller one: a pilot session could commit a command its own readers
refused, leaving a journal clean on disk, invalid to everything that could read
it, and past repair, because nothing was damaged.

Events fall into three classes and the class decides the rule:

- **A human decision** — the head of the four commands: an order submitted, an
  order the trader cancelled, a protection replaced, a protection withdrawn. It
  carries a gesture and a moment, or in a scripted run neither.
- **A presentation** — an interface confirming an observation reached a screen.
  It commands nothing and names no act, but it is the beginning of every
  interval the decisions are measured over, so it belongs to the same
  chronology. A run nobody watched has none at all: not an unstamped one, none.
- **Everything else** — what the events before it required. Nobody commanded
  it, so nobody timed it, and it carries no moment.

The classification reads an event's own reason field for the two kinds that can
be either — a cancellation and an ending are a person's act only when they say
so. That is safe only because `requireOwed` pins the reason of every
cancellation and ending the log demanded. The two are load-bearing together:
without the second, relabelling a derived cancellation as the trader's would
move it into the class permitted to carry a clock.

**Check is pure and Apply follows the record.** A check that advanced the clock
and was then followed by a later refusal would turn a command that left no
events into one that silently moved the chronology — a journal that still looks
perfect with only its intervals wrong. So the door asks before anything decides,
the journal asks again as every event passes, and only an event actually
appended advances the reading. A refusal spends no gesture and moves nothing,
which is what lets a client correct a stamp and send the same act again.

**Equal readings are allowed and the wall clock is never compared.** Two things
can happen at one reading, and a latency of zero is a measurement rather than an
impossibility. A wall clock may legitimately move backwards — a time server
corrects it, an operator sets it, a suspended machine resumes — and refusing a
corrected clock would refuse a session that was entirely honest.

Monotonicity and `requirePresented` are both needed and neither replaces the
other: the first makes a negative interval unrepresentable, the second proves
the subtraction is against the stimulus actually on the screen.

### A working order outlives a session boundary

An order still waiting when a trading session ends is still waiting when the
next one opens, and so is a protection planned against it. They cross the way a
position does: `openTradingSession` re-bases the evaluation's reference and
resets the session's counters, and touches neither. The first book of the new
session is offered to them exactly as the old session's would have been.

This is provisional and it is deliberate. It is not `TimeInForce`: one
behaviour needs no abstraction, and if the pilots turn out to want day orders
that is a typed field and a typed cancellation arrived at on purpose — not a
silent change to what a journal already means.

It has a test rather than only this paragraph, because until it did the
behaviour was an accident: nothing cleared `s.working` because nobody had
written the line, and a paragraph asserting it would have documented an
omission. The next person to add that line would have broken nothing red.

The participant sees it: `project` serves `working` and `protection` from the
session, so an order carried across a boundary is on the screen under the new
session's identifier.

### The journal ends where the evaluation ends

A prop-firm evaluation reaching passed or failed in the middle of a file is the
normal case, not an error. The run stops consuming, cleanly: it does not close
the trading session on the participant's behalf, does not open the next one, and
exits zero, because an evaluation ending is a result rather than a failure.

Market after that point is market nobody can act on, and the file it came from
still holds it, so recording it would be a second copy. It is also not free.
`Consumed` requires every recorded observation to sit in the trading session the
file assigns it, so keeping the later market means crossing the next boundary,
and crossing it means opening a session on an evaluation that is over — the
terminal state of that machine is terminal in both directions, and reopening it
to store a quote nobody can trade against is the wrong trade.

`Consumed` needs nothing for this. It walks events rather than rows, so a
journal shorter than its file is the interruption case it was already written
for.

The rule is in `Session.Observe` and the stop is in `Drive`, and the second is
not a duplicate of the first. The kernel's refusal is what holds the next
adapter honest when an interface starts feeding observations by hand. Drive's
check is at the top of its loop rather than in reaction to that refusal, because
the boundary logic runs before the observation does: reacting would close the
trading session first and commit a `SessionEnded` this policy says should not
exist, so the run would still stop and the journal would grow every time it ran.

`EndTradingSession` keeps no `ended()` gate while `OpenTradingSession` has one.
The asymmetry is deliberate: opening a session on an evaluation that is over
asks the challenge engine to leave a terminal state, and closing one asks
nothing of it. Closing the books after an evaluation ends is a legitimate
operator act, and it was only ever a defect when it happened automatically
inside a run that then could not continue.

**The exit code stays zero, so what was consumed is a line and not a status.**
`praxis replay` prints the outcome and the rows it did not take. That is
deliberate: "the file was consumed whole" is no longer deducible from the exit
status, so anything automating that question reads `remaining:`. Adding a code
for it later would break every caller that had learned to read zero as success.

Known gaps: commission is a flat per-contract figure, not a schedule; a
provider normalizer that turns raw data into the canonical format does not
exist, and needs its own decision record before it does; `AccountSnapshot`
carries neither position size nor any notion of a session having been traded,
which is why the rules needing them are deferred; and the journal records no
planned risk and no protective levels, which is what
[`docs/experiment/power-sensitivity.md`](experiment/power-sensitivity.md)
identifies as the scope of the interface.

## 12. Statistical and commercial guardrails

Freeze hypotheses before collecting sessions; do not rewrite them during data
collection. An underpowered study fails to reject for lack of data, not for
absence of effect: a kill criterion evaluated without a prior power
calculation can retire a true thesis. That calculation now exists in outline —
see [`docs/experiment/power-sensitivity.md`](experiment/power-sensitivity.md) —
and it says the original figure of 50 to 100 sessions was chosen by feel and is
not a sample for an effect measured in R.

Statistical work may use floating point. That is an analytical boundary in the
sense of ADR-002, and it may never reach execution, P&L or challenge code. With a small sample, exploratory patterns are hypotheses, not
findings. Stability across two halves is a minimum guardrail, not proof of
causality or transfer to real-money trading.

Do not build commercial features before completing the personal experiment.
Never market P&L or imply expected returns. Data licensing and financial
promotion requirements are time-sensitive legal questions and must be verified
with the relevant provider and qualified counsel before commercial use.

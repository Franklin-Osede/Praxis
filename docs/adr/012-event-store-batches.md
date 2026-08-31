# ADR-012 — A command is the atomic unit of the event store

Status: accepted.

## Decision

The event store's unit of atomicity is the **batch**: every event one domain
command produced, written and confirmed together or not at all.

```text
[batch length][batch payload][batch checksum]
```

The payload holds the whole event sequence of one command. A batch counts as
confirmed only when it has been written in full and has passed the store's
durability policy.

Per-event framing was considered and rejected. One command produces several
events — an order submitted, a fill, a position change, a valuation, a
challenge decision — and if each were framed independently a process dying
between the second and the third would leave every record individually intact
while the domain operation was cut in half. A checksum cannot see that, because
nothing written is wrong; what is missing is the rest of the command.

## What a command is

A batch corresponds to exactly one of: starting a session, opening a trading
session, accepting an observation, submitting an order, ending a trading
session. A command that produces no events produces no batch; an empty batch is
invalid.

## Fixed decisions

**One writer per journal.** Concurrent appends are not serialised by this
design and must not be attempted. A second writer is a defect, and the store
should refuse to open a journal it does not hold exclusively.

**A canonical, versioned encoding.** The same events encode to exactly the same
bytes, on every run and every machine. Without that, replay equality is a
property of an encoder rather than of the domain, and two runs that agree could
still produce different files.

**Checksums are for accidental corruption.** They detect a partial write, a
torn sector or a flipped bit. They are not a defence against someone editing
the file: anyone able to change the payload can recompute the checksum. Tamper
resistance is not claimed and must not be implied. What defends against a
fabricated history is replaying it against the aggregates, which the session
package already does.

**A reader accepts only complete, valid, sequentially continuous batches.** The
first event of each batch must continue the sequence of the last event of the
previous one. A gap is a corrupt file, not a resumable one.

**An incomplete tail is ignored, never repaired, by a normal read.** A reader
loads up to the last confirmed batch and reports how many trailing bytes it
disregarded. It does not write. A truncated tail is the expected shape of a
process killed mid-append and must not be silently erased by everything that
opens the file.

**Repair is an explicit operation.** Truncating a damaged tail happens only
when asked for, and only after a backup, or with an exact report of the bytes
discarded. A tool that repairs by default destroys the evidence of the defect
that caused the damage.

## Durability is not integrity

Two different properties, and the ADR keeps their names apart.

*Integrity* is length plus checksum: it tells a reader whether the bytes it has
are the bytes that were written.

*Durability* is whether a confirmed batch survives the process or the machine
dying. Without an explicit sync, an append returning no error means only that
the operating system accepted the bytes — not that they reached the disk.

The store therefore carries an explicit durability policy. Only one exists
initially, `DurableEveryBatch`, which syncs before a batch is reported
confirmed. Others may be added when a measured cost justifies one; until then
the guarantee is stated rather than assumed.

## The consequence for Session

The present order — mutate the aggregates, then try to write — cannot survive
a store that can fail. A failed write would leave memory ahead of the disk, and
the next command would be computed from state no confirmed batch contains.

**Decision: a command executes against a copy of the aggregates, its batch is
persisted, and the new state is published only after the commit.** A failed
write leaves the published state exactly as it was, and it is by construction
the state the last confirmed batch describes.

### Alternatives considered

*A pure transition* — `state + command -> new state + events`, with no
mutation until the events are confirmed — is the architecturally cleaner
answer and is what a larger system would do. It is rejected for now because it
means restructuring three aggregates before a second concrete need for it
exists.

*Rebuilding from the confirmed store after a failed write* deserves recording,
because this repository is unusually well placed for it: `Replay` already
reconstructs a whole session from a journal and proves every derived fact
against the aggregates that produced it, so recovery would introduce no new
domain code and inherit tests that already refuse fabricated facts. Copying, by
contrast, means new code whose bugs are silent — a field forgotten in a copy
function produces a wrong result with no error anywhere.

It is not chosen because recovery would cost time proportional to the journal
and would leave the session unusable until it finished, and because a copy is
simpler to reason about at the moment of failure. If copying an aggregate
proves fragile in practice, this is the decision to revisit first.

## Consequences

Every recoverable state corresponds to a complete sequence of domain commands,
not merely to a sequence of intact records. That is the property the CLI and
the UI need before they exist: a session interrupted at any instant reopens on
a boundary the domain itself could have produced.

A crash between two commands is ordinary and expected. A crash inside one is
invisible afterwards, because the partial batch is not loaded.

Copy functions for the aggregates become correctness-critical code with no
other consumer. They must be tested by driving a session, copying, running
further commands against the original, and proving the copy did not move.

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

## The frame

A journal is a text file with a version line and then one frame per batch:

```text
PRAXIS-EVENT-STORE 1
BATCH 0000000001 0000000000000427 0000000000000002 0000000000000006 0000000000000005 CRC32C:8a31f902
<exactly 427 bytes of canonical event text>
```

The header line is, in order: the literal `BATCH`, the batch's monotonic
number, the payload's length **in bytes**, the sequence of its first event, the
sequence of its last, the number of events, and the CRC32C of the payload in
lowercase hexadecimal. Fields are separated by a single space and the line ends
with `\n`. The payload follows immediately and is exactly as many bytes as the
header states.

Each header field is fixed-width, zero-padded decimal. That is deliberately
*not* the payload's integer rule, and the reason is that a header of constant
length is either wholly present or visibly not, which a variable-length one
cannot be.

Length is counted in bytes, never in lines, so a payload truncated anywhere is
detected without hunting for a newline that may not exist. The checksum covers
exactly the persisted payload bytes, its own newlines included.

The metadata is not decoration. It is what lets recovery answer whether a
command was confirmed without re-running it: a batch whose event count,
first sequence or last sequence contradicts its payload is corrupt even though
its checksum matches the bytes present.

## The payload grammar

The payload is UTF-8 text, one event per line, terminated by `\n` and never by
anything the host operating system would prefer. A line is the event's type
followed by its fields, separated by single spaces, in a fixed order per type.

- Integers are canonical decimal: no sign for positives, no spaces, no
  thousands separators, and no leading zeros except for `0` itself.
- Enumerations are written as names, and the codec owns that mapping rather
  than borrowing a `String` method. A display string that changed would
  otherwise silently change the file format.
- Identifiers — session ids, order ids, instrument symbols — are restricted to
  `A-Z a-z 0-9 . _ : -`. That single rule replaces every escaping question, and
  a value outside it is refused when writing as well as when reading.
- A boolean, when one exists, is `0` or `1` and nothing else. None exists yet.
- An unknown field, a missing field, an extra field or an unknown event type is
  an error in version 1. A reader that tolerated them would be guessing.

JSON was considered and rejected. Byte-identical output would need rules for
property order, escaping, whitespace and number representation — all of which
are solvable, and none of which buys anything for nine closed event types.

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
previous one, and batch numbers must be consecutive. A gap is a corrupt file,
not a resumable one.

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

The present order — mutate the aggregates, then try to write — cannot survive a
store that can fail. A failed write would leave memory ahead of the disk, and
the next command would be computed from state no confirmed batch contains.

**Decision: a command executes in memory, its batch is written, and a failed
write is recovered by reconstructing from the confirmed batches.**

```text
execute the command in memory
  -> build its batch
  -> attempt to persist
  -> confirmed: carry on
  -> failed: close and reopen the store, rebuild from the intact batches
```

Reconstruction is chosen over executing against a copy because this repository
is unusually well placed for it. `Replay` already rebuilds a whole session from
a journal and proves every derived fact against the aggregates that produced
it, so recovery introduces no new domain code and inherits tests that already
refuse fabricated facts. A copy would be a second representation of the state
whose only consumer is the transaction, and a field forgotten in a copy
function produces a wrong result with no error anywhere.

The cost is accepted: recovery takes time proportional to the journal, and the
session is unavailable until it finishes. That is a rare path, and being slow
on it is better than being silently wrong on every path.

*A pure transition* — `state + command -> new state + events`, with no mutation
until the events are confirmed — remains the architecturally cleaner answer and
is what a larger system would do. It is not chosen now because it means
restructuring three aggregates before a second concrete need for it exists.

## A failed write is ambiguous, and recovery must find out

An error from a write or a sync does not mean nothing was written. The batch
may be complete on disk and the writer still have been handed an error.

**Recovery discovers whether the command was confirmed; it never re-executes
it.** Re-running a command whose batch did land would duplicate it, and the
duplicate would be indistinguishable from a decision the trader made twice.
This is what the batch number and the event sequence range are for.

After a failed write:

- **The batch is present and intact.** The command was confirmed. Continue from
  it.
- **The batch is absent.** The state returns to the previous commit, as though
  the command had never been issued.
- **The batch is partial, or its metadata contradicts its payload.** The tail is
  ignored and the session is available only from the last intact batch.
- **Reconstruction itself fails.** The session is unusable and says so. It never
  continues with memory and disk diverged, which is the one outcome that would
  make every later guarantee meaningless.

## Consequences

Every recoverable state corresponds to a complete sequence of domain commands,
not merely to a sequence of intact records. That is the property the CLI and
the UI need before they exist: a session interrupted at any instant reopens on
a boundary the domain itself could have produced.

A crash between two commands is ordinary and expected. A crash inside one is
invisible afterwards, because the partial batch is not loaded.

Recovery is on the failure path, so it must be exercised deliberately: a
failing writer injected after every byte offset, a wrong checksum, a batch
whose metadata contradicts its payload, an unknown event, a second writer, and
a sync that reports failure after the bytes landed.

# Praxis

A deterministic trading simulator, written in Go, for measuring how a trader
behaves rather than only what they earned. It replays a recorded market session,
applies prop-firm-style evaluation rules (daily loss limit, drawdown, profit
target), and records every meaningful decision as a domain event in an
append-only journal: orders, cancellations, moved stops, and how long the trader
looked at the market before acting.

I am building it to test one idea with real data: that traders fail mostly at
following their own plan, for example by widening a stop after a losing run, and
that this can be measured. Praxis does not predict markets, generate signals or
trade real money.

## Status

Built and tested on `main`:

- A simulation kernel for one futures instrument (MNQ): conservative order
  execution for market, limit and stop orders, resting orders and partial
  fills, positions with an exact cost basis, account balance and equity, and an
  evaluation state machine with four rules.
- Protective orders (stop and target) as a first-class aggregate:
  planned with an entry, moved, withdrawn, activated by a fill, and executed
  one-cancels-the-other.
- A durable, checksummed event journal, with crash recovery, explicit repair,
  proof of history and anchoring against truncation.
- A command-line tool (`praxis replay`, `praxis store ...`) and a minimal local
  web interface (`praxis ui`) for running manually paced pilot sessions.

Not built yet:

- The confirmatory experiment mode (fixed automatic pacing). It waits until the
  pilot sessions show that the interface records decisions without losing or
  duplicating any.
- Bar-based execution. Worst-case intrabar resolution exists and is tested, but
  no market data adapter supplies bars yet, so only quote files can be run.
- Converting provider data into the canonical market file format.
- Adaptive training, synthetic market generation and live paper trading. These
  are planned only if the pilot experiment finds stable behavioural patterns
  ([`docs/PRAXIS_SPEC.md`](docs/PRAXIS_SPEC.md), section 8).

No pilot sessions have been run, and the experiment's hypotheses are not frozen.

## How it works

**Deterministic by construction.** Prices, money and risk limits are integers
(`Ticks`, `Cents`, `Qty`), never floating point. Time enters as data (logical
time on each observation), never from `time.Now()`. The kernel runs in one
goroutine and never iterates a map where order matters. Session boundaries are
stated by the market file instead of being derived from a calendar
([ADR-011](docs/adr/011-session-boundaries.md)). The same inputs give the same
journal, byte for byte.

**Conservative execution.** The simulator never gives the trader a better price
than the market offered. Market orders fill at or worse than the quote, stops
fill at the touch rather than at their level, gaps resolve against the trader,
and an ambiguous bar resolves to the stop.

**Hexagonal layout.** Domain packages (`market`, `execution`, `portfolio`,
`challenge`, `session`) import no infrastructure: no filesystem, no HTTP. Files,
the journal and the web interface live in `internal/adapters`, behind a few
narrow ports.

**Event-sourced journal.** Every command (submit an order, move a stop, receive
a market observation) produces one batch of events, written as
`[length][payload][checksum]` and confirmed together or not at all
([ADR-012](docs/adr/012-event-store-batches.md)). A batch is synced to disk
before it counts as confirmed, and one process holds the journal under an
exclusive lock. If a commit fails, the session stops for good instead of
guessing what reached the disk.

**Recovery, repair and resume.** On restart, the writer recovers the confirmed
batches and ignores an unfinished tail. `praxis store repair` reports a torn or
corrupt tail and changes nothing unless given `--apply`; when it truncates, it
keeps the removed bytes beside the journal as evidence. A resumed run checks
the market rows already recorded and continues from there, producing the same
journal an uninterrupted run would have. A fuzz test
(`FuzzResumeAtEveryBatch`) drives sessions with arbitrary command sequences
and resumes at every batch to check this.

**Two separate claims about a journal.** `praxis store verify` replays every
event against the aggregates that produced it and fails if the log contains
contradictory facts (coherence). A journal cut at a batch boundary is still
coherent, so completeness needs an external reference: `praxis store anchor`
prints a value binding the run, the last batch and a SHA-256 digest of the
prefix, to be stored where the participant cannot write
([ADR-015](docs/adr/015-anchoring-a-journal-against-truncation.md)). `verify`
can also check that a journal matches the market file it claims to describe.

**Stable formats.** The journal payload has six published versions. Each has a
golden fixture, and the goldens are pinned by a SHA-256 manifest, so changing
one requires changing two files on purpose.

**Tests at several levels.** Table-driven unit tests, property tests (no
impossible favourable fill, cost basis conserved across partial closes, the
same ambiguous bar always resolving the same way), deterministic replay tests
across repeated runs, the fuzz test above, and a test that runs the web page in
headless Chrome through chromedp.

## Repository layout

```text
cmd/praxis/                     the praxis command (replay, ui, store)
internal/market/                instruments, quotes, bars, integer arithmetic
internal/execution/             conservative quote execution, worst-case intrabar
internal/portfolio/             position with exact cost basis, account
internal/challenge/             evaluation rules and state machine
internal/session/               one deterministic run, its events, replay, proof
internal/adapters/marketdata/   canonical market file reader
internal/adapters/persistence/  journal framing, writer, recovery, repair, anchor
internal/adapters/ui/           local HTTP server and embedded web page
docs/                           specification, ADRs, experiment design, learning notes
```

## Requirements

- Go 1.26.5 or later (see [`go.mod`](go.mod)).
- Optional: Chrome or Chromium, for the browser test in `internal/adapters/ui`.
  Without one, that test is skipped and says so. Set `PRAXIS_REQUIRE_BROWSER=1`
  to make a missing browser a failure, and `PRAXIS_BROWSER` to point at the
  executable if it is not in a standard place.

## Build and test

From the repository root:

```bash
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

## Try it

The repository includes a small market file with two trading sessions. Amounts
are in cents.

```bash
go build -o /tmp/praxis ./cmd/praxis

# Run a session over the market file and write a journal.
/tmp/praxis replay internal/adapters/marketdata/testdata/two-sessions.csv \
  --journal /tmp/demo.journal \
  --starting-balance 5000000 --max-daily-loss 100000 --profit-target 300000

# Check the frames, then prove the history and match it to the market file.
/tmp/praxis store inspect /tmp/demo.journal
/tmp/praxis store verify /tmp/demo.journal internal/adapters/marketdata/testdata/two-sessions.csv

# Take an anchor and verify against it.
ANCHOR=$(/tmp/praxis store anchor /tmp/demo.journal)
/tmp/praxis store verify /tmp/demo.journal --anchor "$ANCHOR"

# Simulate a write cut off mid-append, then repair it.
printf 'garbage' >> /tmp/demo.journal
/tmp/praxis store inspect /tmp/demo.journal          # exit 1: incomplete tail
/tmp/praxis store repair /tmp/demo.journal --apply   # truncates, keeps the tail as evidence

# Resuming checks the rows already recorded and adds nothing new.
/tmp/praxis replay internal/adapters/marketdata/testdata/two-sessions.csv --journal /tmp/demo.journal
```

`replay` only feeds market observations. Orders are placed from Go code in
tests, or by a person through the interface:

```bash
/tmp/praxis ui internal/adapters/marketdata/testdata/two-sessions.csv \
  --journal /tmp/pilot.journal --subject t-01 --run-id t-01-s1 \
  --starting-balance 5000000 --commission 50 \
  --max-daily-loss 100000 --profit-target 300000
```

It listens on loopback, prints the address to open and a one-time handover key,
and serves a page compiled into the binary. The operator's procedure for a real
session is in [`docs/experiment/pilot-protocol.md`](docs/experiment/pilot-protocol.md).

### Exit codes

`praxis store` commands use exit codes meant for scripts:

| Code | Meaning |
| --- | --- |
| 0 | Clean, or repaired and validated. |
| 1 | Repairable, and nothing was changed. |
| 2 | Fatal: not a journal, damaged beyond truncation, or an unreadable anchor. |
| 3 | Busy: another process holds the journal. |
| 4 | No such input. |
| 5 | Frames intact, history not provable. |
| 6 | History provable, but not the journal the anchor confirms. |
| 7 | History provable, but it does not describe the market file supplied. |

### Market file format

```text
praxis.market.v1,MNQ,50
time,sequence,session_id,bid,ask,bid_size,ask_size
3000,1,d1,20000,20001,10,10
```

The first line is the format version, the instrument and its tick value in
cents. The header must match exactly. Rows must be in order, and a session
identifier cannot reappear after another session has begun.

## Documentation

- [`AGENTS.md`](AGENTS.md): mandatory working rules (determinism, no floating
  point for money, test-first for domain changes).
- [`docs/PRAXIS_SPEC.md`](docs/PRAXIS_SPEC.md): normative engineering and
  product specification, including the roadmap.
- [`docs/PRAXIS_CONTEXT_ES.md`](docs/PRAXIS_CONTEXT_ES.md): non-normative
  Spanish summary.
- [`docs/adr/`](docs/adr/): architecture decisions. ADR-001 to ADR-009 are
  recorded in section 3 of the specification.
- [`docs/experiment/`](docs/experiment/): power analysis, candidate hypotheses,
  trade identity and the pilot protocol.

New to futures trading? [`docs/learning/`](docs/learning/README.md) explains the
domain from the ground up, with a [glossary](docs/learning/GLOSSARY.md).

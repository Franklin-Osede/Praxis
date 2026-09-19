# The pilot protocol: what the operator does

This is the part of the apparatus that is not software. Two of the record's
claims are backed by an operator rather than by a test — custody of the anchor
(ADR-015) and THE-PAINT-CLAIM (specification, section 11) — and a claim backed by
nobody doing anything is a claim that is not backed.

## Before the first session of a build

1. Build the binary that will run every session, and keep it. The screen is
   compiled into it, so a rebuilt binary is a different apparatus.
2. Check THE-PAINT-CLAIM once, with a devtools trace: the acknowledgement leaves
   after the frame carrying the observation is painted. Write down the browser
   and its version. They belong here and not in the journal: the browser supplies
   them and nothing can contradict them, the same objection that keeps the
   participant's clock out of the record.

## Running a session

    praxis ui <market-file> --journal <journal> --subject <label> --run-id <label> \
      --starting-balance <cents> --commission <cents> \
      --max-daily-loss <cents> --profit-target <cents>

`--run-id` names this execution, and the journal refuses to be written without
one when somebody is trading it. It is supplied rather than generated, because a
generated one would end the byte-for-byte identity of two runs that four
determinism tests rest on.

**Give every session its own label, and never reuse one.** Nothing in the
software can stop a label being typed twice; if it is, two journals claim the
same execution and an anchor can no longer say which of them it certifies. That
is the whole of what the identity buys, so it is protocol discipline in the same
way custody is. A label that carries the subject and the sitting — `t-01-s2` —
makes a reuse visible on the page it is written on.

The console prints a handover key. It stays with the operator: it is what takes
the controls back from a tab that is gone, it works once, and it is reprinted
whenever it is used.

Open the address it prints. Take the controls, advance, confirm, trade. The
participant advances by hand and the gap between two presentations is the record,
so nothing hurries them.

## At collection, with the participant present

    praxis store anchor <journal>
    praxis store verify <journal> <market-file> --anchor <the anchor>

Write down, away from the machine:

| what | where it comes from |
|---|---|
| the anchor | `praxis store anchor` |
| the configuration digest | the `config:` line of `verify` |
| the subject label | the `subject:` line of `verify` |
| the run identity | the anchor's second field, and `--run-id` as given |
| the market file's name and SHA-256 | `shasum -a 256 <market-file>` |
| the browser and its version | the operator's note for this build |
| THE-PAINT-CLAIM checked | the operator's note for this build |
| any repair evidence beside the journal | `ls <journal>.tail-*`, **including its absence** |

**Why the repair evidence is on this list.** A repair truncates a journal back to
its last confirmed batch and keeps what it discarded in a file beside it. An
anchor is taken at collection, which is *after* any repair, so it certifies the
repaired journal as complete and cannot see that anything was discarded — and
the only evidence is a file the person who repaired it could delete. So the
operator lists it and writes down what is there, absence included. A session that
arrives with repair evidence is a session that was interrupted and cut back,
which is a fact about that pilot rather than an operational incident.

**What this covers, and what it does not.** An anchor taken at collection
certifies the journal against alteration *after* collection. It says nothing
about the interval before it; what covers that is the operator being present.
A pre-registration should say exactly this.

## The first session with a person is a rehearsal, and its data is discarded

Declared here, before any session is run, because that is what makes it
legitimate: deciding to discard a session *after* seeing what it contains is a
degree of freedom an analysis cannot have. This one is discarded by design and in
advance, whatever it shows.

What it rehearses is the part no software imposes. The engine has tests; the
steps on this page do not, and they have never been walked with a person sitting
in front of the screen:

- the paint check for this build, done and written down;
- the anchor taken at collection and recorded away from the machine;
- `ls <journal>.tail-*`, with its answer written down either way;
- a run label that is this session's and no other's;
- and the plain question of whether the operator can do all of it while
  somebody waits.

Fourteen useful sessions and one that tests the operator costs less than fifteen
contaminated by a step skipped the first time and noticed at the analysis. The
rehearsal's journal is kept — it is evidence about the apparatus — and excluded
from the sample, and the exclusion is cited to this paragraph rather than
explained afterwards.

## If a session is interrupted

Restart `praxis ui` on the **same** journal, with the same `--subject` and
`--run-id` or with neither: the journal keeps its own, and a label that
disagrees is refused rather than ignored. Never run `praxis replay` on a
journal somebody traded — it advances the market with nobody watching, and the
rows it adds were seen by no one.

If the restart refuses the journal for an unconfirmed tail, repair is the
operator's decision and is taken with the participant present:

    praxis store repair <journal>            # a dry run: it changes nothing
    praxis store repair <journal> --apply

**Check the dry run's `batches: N confirmed` against something written down** —
the batch in the last anchor, or the last `praxis store verify` that exited 0 —
before `--apply`. Not against the dry run's own account of itself: damage to a
length field is exactly what makes that number wrong, and a journal whose
confirmed commands are misreported as an unfinished write is truncated on
ordinary consent. If N is lower than what you wrote down, stop and keep the
file; the discarded bytes survive only in `<journal>.tail-*`, which the repair
writes beside it.

Record the interruption on the collection sheet either way. Sessions lost to
interruption is one of the quantities the pilots exist to estimate, and nothing
in the software counts them.

## A dry run of the whole apparatus

Run once before the first real session, with a fictitious subject, over a small
market file crossing a trading-session boundary. It is the first time every part
runs together, and it is cheaper to find what is missing here than with a person
sitting in front of it.

    take controls        → 200  segment 1
    step + acknowledge   → 200  cursor 1/5  bid 20000 ask 20001  session d1
    step + acknowledge   → 200  cursor 2/5  bid 20010 ask 20011  session d1
    buy 2 with brackets  → 200  net 2  equity 4999800 cents  protections 1
    step + acknowledge   → 200  cursor 3/5  bid 20005 ask 20006  session d1
    step + acknowledge   → 200  cursor 4/5  bid 20020 ask 20021  session d2
    step + acknowledge   → 200  cursor 5/5  bid 20030 ask 20031  session d2
    step                 → 422  feed_exhausted

Then collection, on that journal:

    praxis.anchor.v1 t-01-s1 15 29 sha256:44228356...

    versions:  container 1, payload praxis.event.v5
    condition: clean
    config:    sha256:cc6c807c...
    subject:   t-01
    proved:    29 events replayed against the aggregates that produced them
    anchored:  yes — through batch 15, sequence 29, and the bytes leading to it
               nothing bounds what came after it
    stimulus:  corresponds — the journal describes the supplied file through row 5
               the rows its anchored prefix consumed are bound through the anchor;
               rows consumed after it are not

    $ ls <journal>.tail-*
    no matches

The run above crossed a boundary with a position open, ended on the file running
out rather than on an error, and its journal verified on all three claims. The
anchor's second field is the run identity, which is what lets one session's
anchor be told from another's.

Run it again whenever the binary changes in a way that changes what a journal
holds. This transcript is from `praxis.event.v5`, the version the pilots will
write; an earlier one was recorded against v4 and no longer describes what an
operator will see.

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

**What this covers, and what it does not.** An anchor taken at collection
certifies the journal against alteration *after* collection. It says nothing
about the interval before it; what covers that is the operator being present.
A pre-registration should say exactly this.

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

    praxis.anchor.v1 - 15 29 sha256:1e71af8a...

    condition: clean
    config:    sha256:0ff7c312...
    subject:   t-dry
    proved:    29 events replayed against the aggregates that produced them
    anchored:  yes — through batch 15, sequence 29, and the bytes leading to it
               nothing bounds what came after it
    stimulus:  corresponds — the journal describes the supplied file through row 5
               the rows its anchored prefix consumed are bound through the anchor;
               rows consumed after it are not

The run above crossed a boundary with a position open, ended on the file running
out rather than on an error, and its journal verified on all three claims.

# ADR-015 — A journal proves coherence; only an anchor proves completeness

Status: accepted.

## Decision

`praxis store verify` makes two claims, and it must report them as two:

- **Coherent.** The frames are intact, every batch's checksum matches its bytes,
  and the history in them could have been produced by the account and
  evaluation it describes. This is what `clean` and `proved` mean today, and
  they are not weakened.
- **Complete against an anchor.** This journal is the run the anchor names, and
  it still contains everything the anchor confirmed.

The second claim cannot be made from the journal alone. It requires a reference
kept where the participant cannot write, binding three things:

1. the identity of the execution;
2. the last committed batch and sequence;
3. a digest over exactly the prefix those two name.

An anchor is a value, not a file. Where it is kept, how it travels and how it is
authenticated are custody decisions this ADR does not make. What it does decide
is what an anchor must bind, when it must be published, what it proves, and —
more importantly — what it cannot prove.

## What a truncated journal is

`ReadJournal` validates continuity forward and relatively: `expectedNumber`
starts at 1 and increments, `expectedSequence` follows `LastSequence + 1`, and
the loop ends when the bytes do, returning `TailComplete`.

So a journal cut at a batch boundary is not damaged. It is a shorter journal,
and it is indistinguishable — by construction, not by oversight — from an
execution that legitimately ended there. `ConfigDigest` does not close this and
never claimed to: it digests `SessionStarted`, the first event in the file, so
it is precisely the part that survives any truncation.

This is a property of the format. The reader is behaving correctly, and no
change to the reader can detect it.

## Hash chaining does not help

The natural first answer is to chain the batches: put the previous batch's
digest in the next batch's header. It does not work, and it is worth writing
down why, because it is the answer a reviewer will propose.

**A prefix of a hash chain is a valid hash chain.** Removing the suffix removes
the links that pointed into it. The same argument defeats a terminal seal, a
batch count written at the end, and any other construction stored wholly inside
the journal: whatever protects the suffix is removable with the suffix.

Only a reference outside the artefact can bound its length.

## What an anchor proves, and what it never proves

An anchor at batch *N* with digest *D* proves a **lower bound**: this journal
contains at least *N* batches, and its first *N* batches are exactly the bytes
*D* covers. Truncation below *N* is detected; alteration within the first *N* is
detected.

It proves **no upper bound**. Nothing in the anchor says batch *N+1* never
existed. An act taken after the last published anchor and then removed is
undetectable, and no choice of digest changes that.

Therefore the anchoring cadence *is* the security parameter. It is not a
performance tuning knob:

| cadence | maximum undetectable suffix |
|---|---|
| receipt on delivery | the whole session, if the session is never delivered |
| periodic checkpoint | every act since the last checkpoint received |
| one anchor per committed batch | one batch, and only if it was never acknowledged |

A receipt at the end protects a session that was handed over. It says nothing
about a session that was not.

## When the anchor is published

The anchor for a batch is published **before** the command that produced it is
reported successful to the participant.

This is not a new rule. Section 4 of the specification already says a command
decides nothing before the journal has agreed to record it; this is the same
discipline extended across the trust boundary. Publishing afterwards leaves a
window in which the participant has seen an act succeed that nothing outside
their machine has recorded, and that window is exactly the suffix the previous
section says cannot be recovered.

## The run has no identity yet

An anchor must name an execution. The repository has nothing that does.

`ConfigDigest` identifies a *configuration*: two sessions by the same subject
over the same file with the same rules produce the same digest, byte for byte,
because `SessionStarted` carries no per-execution value. An anchor keyed on it
could be presented against a different session of the same subject.

So a run identifier is a **prerequisite**, not a detail:

- It is **supplied by the operator** with `--run-id` and recorded in
  `SessionStarted` as given. An earlier draft of this said the anchor authority
  issues it; under the custody decided below the authority is the operator, and
  the flag is the seam.
- It is not minted. `crypto/rand` would end the byte-for-byte identity of two
  runs that four determinism tests rest on — the boundary, recovery, replay and
  interface comparisons — and those are what show the kernel is deterministic
  and the interface an adapter rather than a second implementation.
- It is a `praxis.event.v5` payload field, and the only one.
- What it cannot do is stop an operator typing one label twice. Then two
  journals share an identity and the claim empties out, so the protocol carries
  the discipline the way it carries custody. Said here because a field that
  claims more than it holds is the failure this document keeps finding.

The consequence is sharper than "an anchor could be misattributed", and it falls
hardest exactly where the cadence table recommends anchoring most. Two runs of
one configuration over one file are **byte-identical**: `SessionStarted`,
`SessionOpened`, `MarketObserved`, `AccountValued` and `ChallengeDecision` carry
logical time and no wall clock, so nothing separates two executions until the
first human decision. The early anchors a per-batch cadence produces first are
therefore the ones with exactly zero discriminating power.
`TestTwoRunsOfOneConfigurationAreIndistinguishable` kept that as a fact in the
suite rather than an assertion here, and
`TestTwoRunsDifferOnlyByTheirIdentityAndAnAnchorTellsThemApart` is what closed
it: two runs of one configuration recording the same history, differing in the
label alone, and one run's anchor refused against the other.

`AnchorOf` reads the identity from the journal rather than taking it as an
argument — the inverse of the fault A2 reported, where a field was stamped with
whatever a caller passed and checked against nothing. A journal written before
v5 names no run, and then an anchor that names one cannot be settled against it
and says so.

## The market file is a second artefact with the same problem

`marketdata.Consumed` compares the journal against **a** market file, not
against **the** market file. An adversary who can truncate the journal can
substitute the CSV, so verifying the two against each other proves coherence
between two artefacts under the same control. It raises the cost of a forgery —
it must now be consistent in two places — without closing it.

ADR-010's provenance section already requires this and it was never implemented:
*"dataset version or immutable content digest when available."*

### Not in the payload

The obvious place is `SessionStarted`, and it was built there and taken out
again. It fails twice.

It fails the threat model. The participant who can rewrite the journal rewrites
the recorded digest with it, re-encodes, recomputes the checksum, and replays
cleanly: the configuration is an axiom nothing inside the journal can check. Its
value runs through `ConfigDigest`, and `ConfigDigest`'s value runs through a
pre-registration comparison that does not exist as a mechanism. It is a field
whose security depends on the same external reference an anchor already is.

And it fails the system's own properties. A digest recorded when a run starts
binds the rows the run has not consumed yet. That forbids a file from growing,
which replay supports, and it breaks byte-for-byte resumption, because an
interrupted run and the run that finishes it legitimately start from different
prefixes of one file. Against honest error — the wrong file supplied — the
payload field adds nothing either: `Consumed` already refuses it row by row.

### Bound transitively, by the journal's anchor

No second digest is needed at all, and one was built and taken out.

The journal's anchor already binds the stimulus. A `MarketObserved` event
carries every column of the row it came from, and every such event before batch
*N* is inside the bytes the anchor's prefix digest covers. `Consumed` compares
each row of the supplied file with those events field by field — time, sequence,
trading session, both sides of the book, both sizes, and the instrument the
header declares. So an anchor that holds plus a `Consumed` that passes fix the
**content** of every row the anchored prefix consumed, and nothing more is owed.
It is the prefix theorem again, without a second artefact to digest: rows the
journal had not consumed are free to change and to be appended, and the next
anchor covers them.

The attack this has to meet — a row changed in the CSV *and* in the journal's
record of it, so the two describe each other perfectly — is caught by the
journal's anchor alone, because the rewritten observation is inside its prefix.
`TestASubstitutionMadeInBothFilesIsCaughtByTheJournalAnchor` holds both halves:
the pair verifying clean with no anchor, and failing against the journal's.

A digest of the market file's bytes was added to the anchor as a second clause,
through the rows consumed. It was redundant where it was right — the attack
above was already caught, and the clause only relabelled the finding — and false
where it was not: the same file with CRLF line endings parses to identical rows,
changes no price any decision was taken against, and was reported as a
substituted stimulus. Binding bytes that do not change meaning is binding the
wrong thing. `TestTheSameRowsInOtherBytesAreTheSameStimulus` asserts that
respelling the file changes nothing about the answer.

The file's spelling is therefore unchecked, and that is a limitation of the
market reader rather than of the anchor: `marketdata.Read` accepts more than one
spelling of the same rows, where the rest of this tree gives a value exactly one.

## The reader must validate what the writer validated

The live path refuses a crossed quote: `observe` calls `q.Validate()` before
recording. The read path does not — neither `DecodeEvents` nor `Replay` calls
`Quote.Validate()`, and the only `Validate` in the decoder is
`ProtectionRef`'s.

That is the writer/reader asymmetry this project has already been bitten by
once, now in the one event nothing recomputes. A forged batch carrying
`Bid > Ask` decodes and replays. This is separable from the anchor and cheaper:
the decoder validates every domain value it reconstructs, on the same principle
that a decoder must not trust the bytes it reads.

Correspondence with a market file is not a substitute for it. An equally invalid
source must not legitimise an invalid quote.

## Threat model

**In scope.** The participant, who is the subject of the record and has an
interest in how it reads. They hold the journal, the market file and any sidecar
beside them; they can read and rewrite all three; they can recompute CRC32C,
which is a checksum and not a signature; they can truncate at any batch
boundary. They cannot alter what is already held elsewhere.

**Explicitly out of scope.** An adversary who controls the anchor authority's
storage, and a participant running a modified binary. A modified binary defeats
every construction in this document, because it decides what to record in the
first place. The answer there is distribution and attestation, not a file
format, and pretending otherwise would be the same substitution this project
refuses everywhere else.

**Not the threat.** Accidental corruption and interrupted writes. Those are
already handled — `TailIncomplete`, `TailCorrupt`, and a repair that is never
automatic.

## Consequences

- `verify` reports two claims. A journal can be coherent and unanchored, and the
  output must not let one be read as the other.
- An anchored verification needs an anchor as input, so the command grows one:
  `praxis store verify <journal> [<market-file>] [--anchor <ref>]`.
- `Batch` gains its end offset, because a checkpoint anchors a prefix and
  checking one requires the byte boundary of the batch it names.
- **An anchor taken by reading the file is a verification tool, not a
  publication mechanism.** Reading a whole journal once per batch is quadratic
  in its own length — a cost this repository has already paid once and written
  down in `journal.go` — and it cannot honour the publication ordering above in
  any case, because the writer holds the lock at the moment the anchor is due.
  So there are two paths and they are not interchangeable: `AnchorOf` reads a
  file and is for verifying one afterwards; `Writer.Anchor` costs a `Sum` and no
  I/O, and is where an anchor published at cadence comes from. That the two
  produce the same value is the property under test, because two paths to one
  claim drift and this one exists only because the other cannot be called often
  enough.
- The refusal for a journal that does not reach its anchor is named for what was
  observed and not for a cause. Truncation is the expected reason and not the
  only one: an anchor taken from a longer journal reaches the same branch, and
  while a run has no identity nothing can separate the two.
- **An anchor has one written form per value**, because it is carried out of
  the process and typed back in, and because it is compared by equality — so two
  anchors that are the same have to look the same:
  `praxis.anchor.v1 <run|-> <batch> <sequence> sha256:<hex>`, with `Format` and
  `ParseAnchor` exact inverses. Every field reuses a spelling this repository
  already has: `-` for
  an absent identifier as the codec writes one, canonical decimals as `market`
  parses them, lowercase hex as a batch header requires. A value the system
  accepts and cannot write down is the position `ValidIdentifier` exists to
  refuse, and an anchor was in it.
- `praxis store anchor <journal>` emits one and
  `praxis store verify <journal> [<market-file>] --anchor <reference>` consumes it. Neither decides cadence, custody or
  authentication: `anchor` prints to standard output precisely because what
  happens to the line afterwards is the custody question and not the program's
  to answer.
- **The two claims stay two on every surface, including the exit code.** A cut
  journal is provable and incomplete, which is not the finding that its history
  could not have happened, so `verify` answers 6 and not 5. The code is the only
  surface a script reads, and collapsing the distinction there would undo it
  wherever it actually gets consumed.
- **A journal that does not describe the market file supplied with it is a third
  finding, and answers 7.** Its observations are not that file's rows, so its
  decisions were not taken against the prices the file shows. When it does
  describe the file, the output says what binds the correspondence — the
  journal's anchor, or nothing outside the two files — so that "corresponds" is
  never read as "authentic".
- **When several findings hold, the code names the most fundamental, in the
  order 5, 7, 6.** A history that could not have happened; then a record that
  does not match the prices supplied with it, because that changes what every
  decision in it meant; then a journal missing acts, which leaves the meaning of the rest
  intact. The order is written into the command's header rather than left to the
  order of its checks, the way `challenge.Observe` fixes the order of its own
  rules. Every finding that holds is printed: the code carries one, the report
  carries what an operator has to act on.
- **Both claims are made under one lock, in one read.** The participant who can
  cut a journal can also rewrite it between two opens, so a report chaining
  `Prove` and a separate check could describe two different files while neither
  line was false alone. `ProveAgainst` takes the anchor and answers both.
- **This does not close the truncation defect.** An anchor a participant emits
  on their own machine anchors a journal they already control. It closes it only
  to the extent that the reference leaves their control, which is open question
  2 and nothing here.
- An anchor validates itself before a journal is read, on the rule every other
  domain value in this tree follows. A malformed reference must not produce a
  complaint about the journal — when the accusation is the product, that is the
  one direction it must never point.
- A run identifier is a v5 payload field, and the only one. The market file's
  identity is neither a payload field nor an anchor clause, for the reasons in
  its section.
- **The anchor's spelling is a draft until custody is decided, and it freezes
  when the first anchor is published somewhere the participant cannot write.**
  Not "because no bytes exist yet": bytes already exist, printed by
  `store anchor`, but a reference that lives beside the journal it anchors is
  worthless, so nothing has yet relied on one. The day something does is the day
  a second spelling would cost a migration, and that is the day it stops being a
  draft.
- Until an anchor exists, no pilot session is defensible against a motivated
  participant. For the pilots the mitigation is custody — the experimenter runs
  the binary and collects the artefacts — and that is an operational control to
  be written into the protocol, not a property of the software.
- For a commercial session the same reasoning has a different answer: the
  journal does not live on the client at all.

## Custody, for the pilots: operational, and not built

The pilots do not get publication. The property that matters is not that an
anchor sits in another file — it is that it leaves the participant's hands, and
anchoring every batch to a console on the same machine does not buy that. Fifteen
sessions, five subjects, three each: that is the shape of a laboratory, and in a
laboratory what takes the anchor out of those hands is the experimenter, not a
file descriptor.

So the protocol carries the step, at collection:

- `praxis store anchor <journal>`, and the anchor written down away from the
  machine;
- beside it, the `config:` digest and the subject label the same command prints,
  the market file's name and its SHA-256, the browser and its version, and the
  check of THE-PAINT-CLAIM for that build.

`docs/experiment/pilot-protocol.md` is that step, with a dry run of the whole
apparatus recorded against it.

**The residual risk, whole, because it is what makes the decision citable: an
anchor taken at collection certifies against alteration after collection, not
before it.** What covers the interval before it is the experimenter being
present. A pre-registration should declare that and not imply more.

This stops being protocol and becomes engineering again at the first session
that is not collected in person. That needs cadence and real publication, and
`Writer.Anchor` is already waiting at no cost. Not before.

## Open, and to be closed before a mechanism is chosen

1. **Cadence.** Receipt, periodic checkpoint, or per-batch. This sets the
   maximum undetectable suffix and nothing else in the design constrains it.
2. **Custody.** Who holds the anchor, and over what channel it is published.
3. **Authentication.** Whether an anchor is authenticated (HMAC, signature) or
   merely held elsewhere. Held-elsewhere is sufficient against truncation;
   authentication additionally survives the anchor store being readable.
4. **What a missing anchor means operationally** — whether a journal with no
   anchor is excluded, reported, or accepted with a note.

## A constraint on ordering, not on design

`Writer.Anchor` reads the writer's state without synchronisation, as the rest of
`Writer` does, and that is sound while the kernel's single loop is the only
owner. Publication is what changes it: an anchor published from another
goroutine makes `Anchor` a second concurrent reader, and the race stops being
theoretical the moment cadence stops being manual. Whoever implements
publication owns that, and it is cheaper to take the lock then than to add one
now for a caller that does not exist.

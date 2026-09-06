# Candidate hypotheses — not frozen

Not normative. Nothing here is a commitment, and nothing here may reach
execution, P&L or challenge code. Freezing happens after ten pilot sessions
have replaced the guesses in section 5, and not before.

This document exists because
[`power-sensitivity.md`](power-sensitivity.md) showed that what a hypothesis
*is* decides what has to be recorded, and building a recording instrument
before knowing that would be guessing at a schema.

**Centrality first.** A candidate earns its place by representing the thesis —
that traders fail at executing their own strategy — and only then is checked
for whether it can be measured. An affordable question that does not matter
would validate nothing.

---

## Candidate 1 — Widening a stop after a losing run *(provisional primary)*

**Why this question matters.** A stop is the one number a trader fixes while
calm and has to honour while not. Moving it away is the plainest possible
instance of failing to execute one's own plan — it needs no theory about
markets, no interpretation of intent, and it happens at the moment the plan is
being abandoned rather than afterwards. If Praxis measures one thing, this is
the strongest candidate for it.

> Is the proportion of entries whose stop is widened higher among entries
> opened after two consecutive completed losing trades?

| | |
|---|---|
| **Unit** | one closed or terminated trade |
| **Denominator** | trades that began with a valid stop |
| **Numerator** | eligible trades whose stop moved away from the entry at least once |
| **Conditioned group** | entries opened after at least two consecutive losing **episodes** |
| **Control group** | trades opened without that condition |
| **Shape** | two observed proportions (question (b) of `power-sensitivity.md` §5) |

**Widening, defined by direction and not by intent.** For a long position, a
move to a lower price. For a short, to a higher one. It is an arithmetic
comparison against the level the stop already had, not a judgement, so it
belongs in the log rather than in analysis — see ADR-008.

**Each trade counts once**, however many times its stop moved. Without that,
one bad afternoon of repeated widening would dominate the sample and the rate
would describe a single trade rather than a habit.

**Exclusions.** A trade that never had a stop is outside the denominator, not a
zero in it: there was nothing to widen. A stop moved *toward* the entry is not
a widening and is not counted as one either way.

**Needs recorded:** the initial stop, every modification with its direction, and
a count of consecutive losing **episodes** at the moment of the entry. The
existing `ConsecutiveLosses` counts closing legs and is the wrong unit — scaling
out of one bad position in two reductions is one trade abandoned in pieces, not
a streak. ADR-013 adds `ConsecutiveLosingTrades` beside it rather than renaming
it.

---

## Candidate 2 — Removing protection while losing

**Why this question matters.** It is the same failure as candidate 1 in its
most complete form: not moving the line but erasing it. Rarer, and therefore
more expensive to measure, but it is the behaviour that ends accounts.

> Is the proportion of trades higher in which a stop is cancelled without
> replacement while the position is showing an unrealised loss?

| | |
|---|---|
| **Unit** | one trade |
| **Denominator** | trades that had protection and were at some point losing |
| **Numerator** | trades where protection disappeared while unrealised P&L was below zero |

**This needs an atomic replacement command.** Cancelling stop A and placing
stop B is two events, and between them the log would show an interval with no
protection that the trader never intended. Without a single command that
replaces one level with another, the measurement would count intent that did
not exist.

`ReplaceProtection` is that command, and it exists. What is not yet built is
the screen, and the screen can undo this on its own: a "remove stop" button
beside a "set stop" button produces exactly the log this candidate cannot use,
whatever the domain offers. That is why it is written into the interface's
constraints rather than left as an implementation note — see section 11 of
[`../PRAXIS_SPEC.md`](../PRAXIS_SPEC.md).

Meaningful, and harder: "without replacement" has to be defined exactly, and
the definition is the measurement.

**Needs recorded:** the whole life of a stop, a trade identity, the valuation
contemporaneous with each change, and a distinction between cancelling and
replacing.

---

## Candidate 3 — Increasing size after a loss

**Why this question matters.** Recovering a loss by risking more is the
mechanism by which a bad afternoon becomes a blown account. It is central, and
it is the one candidate whose data the journal very nearly holds already.

> Is the next entry more likely to be larger than the previous one after a
> losing trade than after a non-losing one?

| | |
|---|---|
| **Unit** | a transition between two comparable trades |
| **Denominator** | transitions between two comparable trades |
| **Numerator** | transitions where the later entry's quantity exceeds the earlier one's |
| **Groups** | the preceding trade's outcome: losing or not |

**This was called unanswerable and it is not**, once the unit is right. See
[`trade-identity.md`](trade-identity.md): "after a losing trade" has no meaning
if a trade is an entry, because weighted average cost gives an entry no P&L of
its own. Read as "the first entry after an **episode** that closed at a loss",
every term is already in the journal — episode boundaries are `PositionChanged`
with kind `opened` and `closed`, and an episode's realised P&L is the sum over
its closing legs.

**Needs recorded:** nothing new.

---

## The denominator, before anything else

A rate is only as real as what it divides by, and a denominator is a
*reconstructible state*, not an event. If it cannot be rebuilt from the journal,
the candidate is not viable however good the question sounds.

| candidate | denominator | is it a state the journal can rebuild? |
|---|---|---|
| 1 | entries that placed a stop | Yes, once `ProtectionPlaced` exists. Counting them needs nothing else |
| 2 | entries whose stop existed and whose episode was at some point losing | Yes: protection events for the first half, and `AccountValued`, which is already recorded after every observation, for the second |
| 3 | transitions from a closed episode to the next entry | Yes today. `PositionChanged` already marks both ends |

None of the three needs a state the journal cannot hold. Candidate 2's is the
most demanding, because "at some point losing" is a property of the whole
interval and has to be evaluated against every valuation inside it rather than
at one instant.

**The denominator counts episodes, never observations.** That is now a decision
and not a preference. An observation-shaped denominator — "every moment a
losing position had a stop that could have been moved" — is reconstructible,
but it is a property of the data file rather than of the trader: `AccountValued`
fires at the feed's observation rate, so doubling the tick rate of the scripted
CSV halves every measured rate, and a pre-registered threshold would be a
statement about a CSV. An episode counts once, however long it lasted and
however many observations it spanned. See `power-sensitivity.md` §7.

## What each candidate needs that the journal does not hold

| | entry identity | initial stop | stop changes | replace vs cancel | valuation at the change |
|---|---|---|---|---|---|
| 1 | already there | required | required | not required | not required |
| 2 | already there | required | required | **required** | already there |
| 3 | already there | — | — | — | — |

**Three events suffice for all three candidates**, and they now exist —
`ProtectionPlaced`, `ProtectionReplaced` and `ProtectionEnded`, in
`praxis.event.v3`. What they are *not* is three fields on an entry, which is
what this section and [`trade-identity.md`](trade-identity.md) both predicted.
Building them found three holes that argument could not have seen, and
[ADR-014](../adr/014-protection-is-an-aggregate.md) is where they are settled:
a planned protection can be moved and withdrawn while its entry waits, a
protective fill needs an order of its own to belong to, and execution is a
reason for a transition rather than a state.

**The consequence for measurement.** A protection is named by its entry while
it is planned and by its episode once a fill has activated it — because once
active the entry no longer governs it, and a second entry adding to the same
position has its plan ended rather than applied. So a `ProtectionReplaced` on
an active protection carries an `EpisodeID`, not the entry's identifier.

Candidates 1 and 2 are written on the entry unit, and attributing a widening
back to the entry that placed the level is therefore a join — episode id, to
the sequence of the `PositionChanged` that opened it, to the `FillProduced`
before it, to its `OrderID` — rather than reading a field. That is
reconstructible and deterministic, but it is work, and it has one case with no
answer in the unit as defined: when a second entry adds to an already protected
episode, its plan is ended with `ProtectionAlreadyActive`. It placed a stop; the
system absorbed it. Whether it belongs in "entries that placed a stop" is a
protocol decision, and the cheaper answer is to write both candidates on the
episode unit, where the question does not arise.

## What candidate 1 costs

A comparison of two observed proportions, both estimated inside the experiment.
The scarce group is the conditioned one; controls come free. Correlation 0.1,
80% power, single sample. Cells are sessions.

**Control 15% widening → conditioned 30%** — 121 eligible trades per group:

| trades/session | conditioned in 10% | 15% | 20% | 30% |
|---|---|---|---|---|
| 3 | 482 | 322 | 241 | 161 |
| 5 | 338 | 225 | 169 | 113 |
| 8 | 257 | 171 | 129 | 86 |
| 12 | 211 | 141 | 106 | 71 |

**Control 15% → conditioned 35%** — a larger effect, 73 per group:

| trades/session | conditioned in 10% | 15% | 20% | 30% |
|---|---|---|---|---|
| 3 | 290 | 194 | 145 | 97 |
| 5 | 203 | 136 | 102 | 68 |
| 8 | 154 | 103 | 77 | 52 |
| 12 | 127 | 85 | 64 | 43 |

Of 192 enumerated combinations of trades per session, conditioning rate, rate
pair and correlation, 59 reach 100 sessions. The cheapest needs 21 — twelve
trades a session, a condition firing on three trades in ten, a rate moving from
15% to 35%, and no within-session correlation. None of those four is known yet.

**The split-half price**, at eight trades a session, a 20% conditioning rate and
15% → 30%:

| | sessions |
|---|---|
| one sample, 80% | 129 |
| both halves, each at 80% (joint ≈ 64%) | 257 |
| both halves, joint 80% | 336 |

**The protocol will mean joint 80%** — 2.63 times the single-sample figure. The
rule exists to stop noise being reported as a finding, and the halves are
advertised as the evidence. If passing it is only 64% likely when the effect is
real, then a true effect is retired one time in three, which is precisely the
"failed for lack of data, not for absence of effect" failure the sensitivity
table was written to prevent. The number is larger and it is the honest one;
whether it is reachable at all is what the pilots are for, and if it is not, the
answer is a different hypothesis rather than a weaker rule.

---

## What the pilots must measure

Every number above is a guess. Ten pilot sessions, excluded from the
confirmatory sample, exist to replace five of them:

1. trades per session;
2. how often a trade is opened after two consecutive losing closes;
3. how often a stop is widened at all — the control rate;
4. the within-session correlation of widening;
5. how many trades begin with a stop, which is the denominator's size.

Only then is the power recalculated and the protocol frozen.

---

## The order this implies

1. ~~Document the candidates.~~ This file.
2. **Choose provisionally.** Candidate 1, not frozen.
3. **Define a trade's identity and its ending.** All three candidates need it
   and none can be measured without it. Partial closes, additions and flips are
   what make it hard, and they are the cases that matter.
4. **Resting orders, and keeping a partially filled remainder.** This precedes
   protection rather than following it: a stop *is* an order that waits for
   later observations. Modelling protection on a kernel that cannot hold a
   waiting order would produce events the engine does not honour — a log
   recording a decision the system ignored, which is the one failure a
   behavioural instrument cannot have. The unfilled remainder that
   `ExecuteOnQuote` documents as "the caller's to carry" and that its only
   caller drops is the same defect, and is fixed here.
5. **The life of a protective level**: place, replace, cancel, trigger.
6. **Carry those into the journal, the codec and `Replay`.**
7. **The minimal UI**, over those commands.
8. **Ten pilot sessions**, to measure the five unknowns above.
9. **Recalculate the power and freeze the hypothesis.**

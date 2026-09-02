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

> Is the proportion of trades whose stop is widened higher among trades opened
> after two consecutive losing closes?

| | |
|---|---|
| **Unit** | one closed or terminated trade |
| **Denominator** | trades that began with a valid stop |
| **Numerator** | eligible trades whose stop moved away from the entry at least once |
| **Conditioned group** | trades opened after at least two consecutive losing closes |
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

**Needs recorded:** a trade identity, the initial stop, every modification with
its direction, the position's direction, the moment of opening, and the
behavioural context at that moment.

---

## Candidate 2 — Removing protection while losing

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

Meaningful, and harder: "without replacement" has to be defined exactly, and
the definition is the measurement.

**Needs recorded:** the whole life of a stop, a trade identity, the valuation
contemporaneous with each change, and a distinction between cancelling and
replacing.

---

## Candidate 3 — Increasing size after a loss

> Is the next entry more likely to be larger than the previous one after a
> losing trade than after a non-losing one?

| | |
|---|---|
| **Unit** | a transition between two comparable trades |
| **Denominator** | transitions between two comparable trades |
| **Numerator** | transitions where the later entry's quantity exceeds the earlier one's |
| **Groups** | the preceding trade's outcome: losing or not |

**`power-sensitivity.md` §3 marked this answerable. That was too generous**, and
this document corrects it. A *trade* is not modelled. It can be inferred where
a position opens and closes cleanly, but partial closes, additions and flips
make the inference ambiguous — and the ambiguity is not rare, it is exactly
what a trader under pressure does. It is not answerable until a trade's
identity and its ending are defined.

**Needs recorded:** a trade identity, which entry belongs to it, its initial
quantity and its terminal outcome.

---

## What each candidate needs that the journal does not hold

| | trade identity | initial stop | stop changes | replace vs cancel | valuation at the change |
|---|---|---|---|---|---|
| 1 | required | required | required | not required | not required |
| 2 | required | required | required | **required** | required |
| 3 | required | — | — | — | — |

All three need a trade identity. Candidate 1 needs the least beyond it, which
is one of the reasons it is provisionally first.

### The correction this forces upstream

`power-sensitivity.md` §3 counted four of six candidates answerable today. Three
of those are — B (time to the next order after a losing close), C (order
frequency once the day is down) and F (a rule breached on a day already down) —
because each is measured at the level of a close leg or a session, neither of
which needs a trade to be defined. The fourth, E (size after a loss versus after
a win), is candidate 3 here and is **not** answerable until trade identity
exists.

---

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

Which of the last two the protocol means is still open, and has to be settled
before freezing.

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

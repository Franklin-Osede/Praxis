# Power sensitivity — under what assumptions is the experiment possible?

Not normative. Nothing here is frozen, and nothing here may reach execution,
P&L or challenge code.

**This document uses decimals.** Effect sizes, dispersions and probabilities
are statistical quantities, not money. That is an analytical boundary, in the
sense of ADR-002: floating point is permitted here and forbidden everywhere it
could touch a price, a balance or a risk limit.

---

## 1. What the words mean

Skip this if you already know them. They are the whole argument.

**The conditioning event.** The thing that has to happen before we start
watching. "Two losing trades in a row." It is not the thing being measured; it
is what splits the data into *after this* and *not after this*.

**The outcome.** The number measured once the condition has occurred. It has to
be a number, not an impression.

**The effect.** How much the outcome differs between the two groups. Small
effects need a lot of data. Big ones do not.

**Dispersion.** How much the outcome varies anyway, for reasons that have
nothing to do with the condition. This is the enemy. A trading result swings
wildly between trades no matter what mood you were in, and every bit of that
swing has to be seen through before an effect becomes visible.

**Power.** The probability of finding an effect that is really there. 80% power
means that if the effect exists, one experiment in five still misses it. That
missed one looks exactly like "there is no effect."

**Within-session correlation.** Ten trades taken on one afternoon are not ten
independent facts. The same mood, the same volatility, the same day. If they
are treated as independent, the experiment believes it has far more evidence
than it does. A correlation of 0.3 across five trades a session means five
trades carry roughly the information of two and a half.

**R.** A result divided by the risk deliberately taken on that trade. Making
1.5 times what you were willing to lose is +1.5R. It is the only way to compare
a small trade with a large one — and, as section 3 shows, Praxis cannot
currently compute it.

---

## 2. The question, put correctly

Not "are a hundred sessions enough?" but:

> Under which assumptions would a hundred sessions be enough, and are those
> assumptions plausible?

The arithmetic below is the standard comparison of two means, inflated for
clustering:

```text
observations per group = 2 (z(1-a/2) + z(power))^2 / (effect / dispersion)^2
design effect          = 1 + (trades per session - 1) * correlation
sessions               = observations per group * design effect
                         / (trades per session * min(rate, 1 - rate))
```

Two-sided at 5%. The design effect is the between-cluster one, which is
conservative here: comparing conditioned with unconditioned trades *inside* the
same session cancels part of the day's shared effect. Erring toward needing
more data is the right direction to err in.

---

## 3. Candidate hypotheses, and whether the journal can answer them

Not frozen. The point of this table is the right-hand column.

| # | Candidate | Unit | Needs | In the journal today? |
|---|---|---|---|---|
| A | After two losing closes, the next entry risks at least 0.3R more | trade | planned risk per trade | **No** |
| B | After a losing close, the wait before the next order shortens | trade | order times, realised P&L per close | Yes |
| C | Orders are submitted more often once the day is down beyond a threshold | trade | session realised P&L, order events | Yes |
| D | A stop is widened rather than honoured while a position is losing | decision | protective levels and their changes | **No** |
| E | Position size in contracts is larger after a loss than after a win | trade | order quantity, preceding realised P&L | Yes |
| F | A rule is breached more often on days already down | session | challenge decisions, valuations | Yes |

Three of the six — B, C and F — are answerable from what `OrderSubmitted`,
`PositionChanged` and `ChallengeDecision` already record, because each is
measured at the level of a close leg or a session, neither of which needs a
trade to be defined.

E is marked yes above and that is too generous. It needs to know which *trade*
lost, and a trade is not modelled: it can be inferred where a position opens and
closes cleanly, but partial closes, additions and flips make the inference
ambiguous, and those are exactly what a trader under pressure does. See
[`hypotheses-candidates.md`](hypotheses-candidates.md), candidate 3.

A and D are not answerable at all. All of A, D and E need the same first thing:
a trade identity.

**What is missing, precisely.** To express any outcome in R, or to say anything
about stops, the log would need: a trade's identity across its entry and exit,
the protective levels placed with it, the money deliberately risked, and every
later modification or cancellation of those levels. None of that exists. An
order and its fill are not enough.

Those fields are not being added now. This table is how the experiment decides
what the interface has to record, and that decision belongs to section 7.

---

## 4. How many sessions an effect-size hypothesis needs

Dispersion 1R, power 80%, within-session correlation 0.1. The cells are
sessions.

| trades/session | condition occurs in | 0.2R | 0.3R | 0.5R |
|---|---|---|---|---|
| 3 | 5% | 3,140 | 1,396 | 503 |
| 3 | 10% | 1,570 | 698 | 252 |
| 3 | 20% | 785 | 349 | 126 |
| 3 | 30% | 524 | 233 | 84 |
| 5 | 5% | 2,198 | 977 | 352 |
| 5 | 10% | 1,099 | 489 | 176 |
| 5 | 20% | 550 | 245 | 88 |
| 5 | 30% | 367 | 163 | 59 |
| 8 | 5% | 1,668 | 742 | 267 |
| 8 | 10% | 834 | 371 | 134 |
| 8 | 20% | 417 | 186 | 67 |
| 8 | 30% | 278 | 124 | 45 |
| 12 | 5% | 1,374 | 611 | 220 |
| 12 | 10% | 687 | 306 | 110 |
| 12 | 20% | 344 | 153 | 55 |
| 12 | 30% | 229 | 102 | 37 |

### What each assumption costs on its own

Holding the rest at five trades a session, a 10% conditioning rate, a 0.3R
effect, 1R dispersion and 80% power:

| correlation | sessions | | dispersion | sessions | | power | sessions |
|---|---|---|---|---|---|---|---|
| 0 | 349 | | 0.75R | 275 | | 80% | 489 |
| 0.1 | 489 | | 1R | 489 | | 90% | 654 |
| 0.3 | 768 | | 1.5R | 1,099 | | | |
| 0.5 | 1,047 | | 2R | 1,954 | | | |

Dispersion dominates everything. Doubling it quadruples the sample.

### The verdict across the whole grid

All 1,536 combinations of the scenarios above:

| sample | combinations reached |
|---|---|
| 25 sessions | 11 of 1,536 |
| 50 sessions | 46 of 1,536 |
| 100 sessions | 139 of 1,536 |
| 200 sessions | 304 of 1,536 |
| 400 sessions | 548 of 1,536 |

These are counts of enumerated cells, not probabilities. The grid is a way of
seeing which assumptions dominate, not a forecast.

The most favourable corner — a 0.5R effect, tight 0.75R dispersion, twelve
trades a session, a condition firing on three trades in ten, no within-session
correlation — needs 10 sessions. The least favourable needs 28,020.

**A hundred sessions reaches the target in 139 of the 1,536 enumerated
combinations.** That is not a probability of feasibility: the grid was chosen by
hand, its scenarios carry no weights, and nothing here says which of them the
real world resembles. What it does show is that the figure in the specification
was chosen by feel, and that plausible assumptions exist in quantity under which
it is not a sample for an effect measured in R.

---

## 5. A frequency question is cheaper, and there are three of them

"How often does this happen?" costs less than "how much does this change the
result?" — but not for the reason it is tempting to give. A rate is not free of
dispersion: a binary outcome has variance `p(1-p)`, and the formulas below use
exactly that. The advantage is that the variance is **bounded and fixed by the
rate itself**, between 0 and 0.25, instead of inheriting the wide continuous
spread of a result measured in R, where a dispersion of 2R quadruples the
sample against 1R.

The saving is real. The reason has to be stated correctly or the next person
will apply it where it does not hold.

### Three different questions, three different costs

They are easy to conflate and they are not the same experiment.

**(a) One proportion against a threshold fixed in advance.** "Stops are widened
on more than 15% of the occasions where they could be." The 15% is stated
before any data is collected and is not estimated from it.

**(b) Two proportions, both observed.** "Stops are widened more often on days
already down than on days that are not." Both rates come out of the experiment,
both carry error, and the comparison costs roughly four to five times the
first.

**(c) A paired or within-session comparison.** The same day contributes to both
sides, which removes the between-day variation and can cost less than (b) — but
it needs a design that pairs the observations, and it is not what the formula
below computes.

| comparison | (a) one-sample, total | (b) two-sample, per group | (b) total | (b)/(a) |
|---|---|---|---|---|
| 15% vs 25% | 114 | 250 | 500 | 4.4x |
| 15% vs 30% | 53 | 121 | 241 | 4.6x |
| 15% vs 40% | 20 | 49 | 98 | 4.9x |
| 20% vs 35% | 63 | 138 | 276 | 4.4x |
| 30% vs 50% | 44 | 93 | 186 | 4.3x |

In sessions, at five trades a session and a correlation of 0.1, for a rate
moving from 15% to 30%:

| condition occurs in | (a) one-sample | (b) two-sample |
|---|---|---|
| 10% of trades | 147 | 338 |
| 20% of trades | 74 | 169 |
| 30% of trades | 49 | 113 |
| 50% of trades | 30 | 68 |

The 49 quoted in earlier drafts of this document was question (a). Most
behavioural hypotheses worth asking are shaped like (b) — they compare a
condition against its absence — and cost between two and three times that.
Quoting the cheap figure for the expensive question would answer something
nobody asked.

## 6. The split-half rule costs more than doubling

The specification requires an effect to hold in both halves of the sample
separately. Doubling the sample gives each half the same power the whole would
have had — and that is **not** the same as the experiment having that power.

If each half is at 80% and both must come out positive, the chance of passing
the rule is roughly `0.80 x 0.80 = 0.64`, assuming the halves are independent.
A one in three chance of missing an effect that is really there.

For 80% **joint** power each half needs about 89.4%, and for 90% joint, 94.9%:

| what the target means | each half | total sample |
|---|---|---|
| each half at 80% (joint ≈ 64%) | 80.0% | 2.00x |
| joint 80% | 89.4% | 2.63x |
| joint 90% | 94.9% | 3.29x |

Worked, for the 0.3R example at five trades a session, a 10% rate and 1R
dispersion:

| | sessions |
|---|---|
| one sample, 80% | 489 |
| both halves, each at 80% (joint ≈ 64%) | 977 |
| both halves, joint 80% | 1,283 |

This is not an argument against the rule. It is an argument for saying which
of the two the protocol means, before it is frozen.

## 7. What this changes

**Centrality first, cost second.** A hypothesis is not chosen because it is
affordable. It is chosen because it represents the thesis — that traders fail
at executing their own strategy — and only then checked for whether it can be
measured with reasonable power. An easy question that does not matter would
validate nothing, and answering it confidently would be worse than answering
nothing.

**Given that, the primary hypothesis is more likely to be a frequency than an
effect size.** Section 4 shows an effect in R is out of reach at any realistic
sample. Section 5 shows a frequency is reachable, provided it is question (a)
or a modest (b). Effect-size questions become exploratory: reported with their
intervals and never claimed as findings.

**Two of the six candidates cannot be answered at all today**, and both need the
same missing thing: a trade's planned risk and its protective levels. That is
the scope the interface has to cover, and it is a much narrower answer than
"record everything."

**But protection events alone may not be enough**, and this is the question to
settle before building anything. A rate needs a denominator. "Stops are widened
on 30% of occasions" requires knowing how many occasions there *were* — every
moment a losing position had a stop that could have been moved and was not.
That is not an event; it is a state that has to be reconstructible. Depending
on which comparison section 5 settles on, the log may also need explicit
relations between a trade's entry, its protection and its exit.

Deciding the exact comparison decides whether the next slice is three events or
something larger.

## 8. What the pilot sessions are for

Ten sessions, labelled as pilots and **excluded from the confirmatory sample**.
They are not evidence and no hypothesis is tested on them. They exist to
replace guesses with measurements in five places:

1. trades per session;
2. how often each candidate condition actually fires;
3. the dispersion of the chosen outcome;
4. the within-session correlation;
5. how many sessions are lost to interruption or error.

Only then is the real calculation done, and only then is the protocol frozen:
the primary hypothesis, the minimum effect worth caring about, the analysis,
the power target, the sample size, the exclusion rules and the stopping rule.

Using pilot sessions as data would be choosing the hypothesis after seeing the
answer, which is the failure this whole document exists to avoid.

---

## Reproducing these numbers

```python
import math
Z = {0.80: 0.8416212336, 0.894: 1.2504, 0.90: 1.2815515655, 0.949: 1.6322}
ZA = 1.9599639845  # two-sided 5%

def sessions(effect, dispersion, trades, rate, correlation, power):
    d = effect / dispersion
    per_group = 2 * (ZA + Z[power])**2 / (d*d)
    design = 1 + (trades - 1) * correlation
    return per_group * design / (trades * min(rate, 1 - rate))

# (a) one observed proportion against a threshold fixed in advance
def one_proportion(p0, p1, power):
    return ((ZA*math.sqrt(p0*(1-p0)) + Z[power]*math.sqrt(p1*(1-p1)))**2
            / (p1 - p0)**2)

# (b) two observed proportions, per group
def two_proportions(p1, p2, power):
    pbar = (p1 + p2) / 2
    return ((ZA*math.sqrt(2*pbar*(1-pbar))
             + Z[power]*math.sqrt(p1*(1-p1) + p2*(1-p2)))**2
            / (p1 - p2)**2)

# each half's power, for a joint target across both halves of the sample
def half_power(joint):
    return math.sqrt(joint)
```

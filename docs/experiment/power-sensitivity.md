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

Three of the six are answerable from what `OrderSubmitted`, `PositionChanged`
and `ChallengeDecision` already record. Two are not, and they are the two the
original hypotheses in the specification were written around.

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

| sample | scenarios it can answer |
|---|---|
| 25 sessions | 11 of 1,536 — 1% |
| 50 sessions | 46 of 1,536 — 3% |
| 100 sessions | 139 of 1,536 — 9% |
| 200 sessions | 304 of 1,536 — 20% |
| 400 sessions | 548 of 1,536 — 36% |

The most favourable corner — a 0.5R effect, tight 0.75R dispersion, twelve
trades a session, a condition firing on three trades in ten, no within-session
correlation — needs 10 sessions. The least favourable needs 28,020.

**A hundred sessions answers one plausible scenario in eleven.** The figure in
the specification was chosen by feel, and for an effect measured in R it is not
a sample.

---

## 5. A frequency hypothesis costs a fraction of that

"How often does this happen?" is a much cheaper question than "how much does
this change the result?", because a rate has no dispersion of its own to see
through.

Observations of the conditioning event needed to distinguish one rate from
another:

| if the true rate is | and we want to detect | 80% power | 90% power |
|---|---|---|---|
| 15% | 25% | 114 | 158 |
| 15% | 30% | 53 | 74 |
| 15% | 40% | 20 | 29 |
| 10% | 25% | 41 | 59 |
| 20% | 35% | 63 | 87 |
| 30% | 50% | 44 | 60 |

In sessions, at five trades a session and a correlation of 0.1:

| hypothesis | condition occurs in | sessions |
|---|---|---|
| 15% → 30% | 10% of trades | 147 |
| 15% → 30% | 20% of trades | 74 |
| 15% → 30% | 30% of trades | 49 |
| 15% → 40% | 20% of trades | 28 |
| 15% → 40% | 30% of trades | 19 |

Comparable questions, one to two orders of magnitude cheaper.

---

## 6. The split-half rule doubles whatever the answer is

The specification requires an effect to hold in both halves of the sample
separately. If each half must stand on its own, each half needs full power, and
the totals double:

| | one sample | both halves |
|---|---|---|
| 0.3R effect, 5 trades, 10% rate, 1R dispersion | 489 | 978 |
| frequency 15% → 30%, condition in 30% of trades | 49 | 98 |

This is not an argument against the rule. It is an argument for knowing its
price before promising it.

---

## 7. What this changes

**The primary hypothesis should be a frequency, not an effect size.** It is the
only shape of question a personal experiment of realistic length can answer,
and section 5 shows it is answerable at 50 to 100 sessions rather than 500.
Effect-size questions become exploratory: reported with their confidence
intervals and never claimed as findings.

**Two of the six candidates cannot be answered at all today**, and both need the
same missing thing: a trade's planned risk and its protective levels. That is
the scope the interface has to cover, and it is a much narrower answer than
"record everything."

**Recording a stop when it is placed and again when it is moved is worth more
than anything else** on the list. It makes candidate D possible, it makes R
computable, and it is a frequency question — the cheap kind.

---

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
Z = {0.80: 0.8416212336, 0.90: 1.2815515655}
ZA = 1.9599639845  # two-sided 5%

def sessions(effect, dispersion, trades, rate, correlation, power):
    d = effect / dispersion
    per_group = 2 * (ZA + Z[power])**2 / (d*d)
    design = 1 + (trades - 1) * correlation
    return per_group * design / (trades * min(rate, 1 - rate))

def observations_for_rate(p0, p1, power):
    return ((ZA*math.sqrt(p0*(1-p0)) + Z[power]*math.sqrt(p1*(1-p1)))**2
            / (p1 - p0)**2)
```

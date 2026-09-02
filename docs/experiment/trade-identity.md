# What is a trade?

Not normative until it is folded into the specification. Nothing here changes
how money is accounted.

Every candidate hypothesis needs it and none can be measured without it, so it
is the decision that blocks the rest.

---

## The difficulty

`Position` is a **net**. Section 4 of the specification settles that a partial
close removes a proportional share of the cost basis using weighted average
cost, deliberately and not FIFO, because prop firms report on average price and
lot tracking buys no behavioural insight.

The consequence is that **an exit corresponds to no particular entry**. Ask
"was that trade a winner?" of a position that was entered three times and
reduced twice, and the question has no answer in the accounting, because the
accounting was designed not to have one.

So a definition of "trade" cannot come from the money. It has to come from
somewhere the money is not.

## Three definitions, and why two of them are wrong

**Flat-to-flat episode.** A trade begins when a position leaves flat and ends
when it returns. Additions and partial closes happen inside it, and a flip ends
one and begins another. Unambiguous, and derivable from what is already
recorded. Its weakness is that a trader who scales in and out across an
afternoon has one enormous "trade", and "the size of the next entry" has no
meaning inside it.

**Entry-anchored lots (FIFO).** Each entry opens a lot; exits consume lots in
order; a trade is a lot. It matches how a trader talks. It is **rejected**: it
requires a second accounting of the same position, running alongside the
weighted-average one, and the two would have to agree forever. That is exactly
the two-contradictory-truths problem that `Verify` exists to police, and it
would contradict a settled decision to buy a convenience.

**Order-bounded episodes.** A trade opens with an exposure-increasing order and
closes when exposure returns to its previous level. Different bookkeeping,
identical objection: it needs lots by another name.

## The decision

A trade is **not one thing**, and trying to make it one is what makes this hard.
Two units already exist in the journal, and each answers a different question.

**An episode** is flat-to-flat. It is where money lives: an episode has a
realised P&L, because it is the only span over which weighted average cost
gives an unambiguous answer.

**An entry** is a single exposure-increasing decision. It is where behaviour
lives: it has a moment, a quantity, a context, and — once protection exists —
the levels placed with it. It has **no P&L of its own**, and asking for one is
the mistake that leads to FIFO.

> Protective levels attach to a decision, not to inventory.

That sentence is the whole resolution. A stop does not need to know which
contracts it protects; it needs to know which decision placed it. Once that is
accepted, no lot tracking is required, no second accounting appears, and the
weighted-average decision stands untouched.

## Both units are already in the journal

Nothing has to be invented, which is the part worth checking before building
anything:

| | already recorded as | since |
|---|---|---|
| episode start | `PositionChanged` with kind `opened` | Phase 1 |
| episode end | `PositionChanged` with kind `closed` | Phase 1 |
| a flip | `closed` then `opened` — two episodes, correctly | Phase 1 |
| entry identity | `Order.ID` on `OrderSubmitted` | Phase 3 |
| the decision's context | `OrderContext` | Phase 3 |
| an episode's realised P&L | the sum of `RealisedCts` over its `reduced` and `closed` legs | Phase 1 |

A position that closes to zero stays flat rather than being deleted — also
settled in section 4 — so an episode's end is a fact in the log and not an
absence to be inferred.

**What is missing is only the reference from a protective level to the entry
that placed it.** That is one field, not a lifecycle relation.

## What each candidate measures, and on which unit

| candidate | unit | denominator | reconstructible today? |
|---|---|---|---|
| 1 — widening after losses | **entry** | entries that placed a stop | after protection events exist; nothing else needed |
| 2 — removing protection while losing | **entry** | entries whose stop existed and whose episode was at some point losing | needs protection events **and** a contemporaneous valuation, which `AccountValued` already records |
| 3 — size after a loss | **episode → entry** | transitions from a closed episode to the next entry | yes, once "after a losing one" is read as "after a losing *episode*" |

Candidate 3 is answerable after all, and the reason it looked unanswerable is
that it was being asked of the wrong unit. "After a losing trade" has no meaning
if a trade is an entry, because an entry has no P&L. Read as "the first entry
after an episode that closed at a loss", every term is already in the journal.

## The consequence for the next slice

**Three events suffice.** `ProtectionPlaced`, `ProtectionReplaced`,
`ProtectionCancelled` — plus `ProtectionTriggered` when a level fires, which is
a fill and not a new kind of fact. Each references the entry's order id. No
trade aggregate, no lot table, no second accounting, and no relation table
joining entry to exit.

Replace is one event rather than a cancel followed by a place, because
candidate 2 measures intervals without protection and two events would
manufacture one the trader never intended.

What still has to be built before them is unchanged: a stop is an order that
waits for later observations, so resting orders come first.

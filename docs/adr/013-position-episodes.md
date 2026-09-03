# ADR-013 — A trade is a position episode

Status: accepted.

## Decision

A trade is the span during which the net position in one instrument stays
non-zero and keeps its direction. It is called a **position episode** in the
code, and may be called a trade in an interface.

```text
flat  -> long           opens an episode
long  -> longer         the same episode
long  -> less long      the same episode
long  -> flat           ends the episode
long  -> short          ends the long episode and opens a short one
session boundary        changes nothing
end of the data         does not end it
a terminal challenge    does not liquidate it and does not end it
```

The internal name is deliberate. "Trade" is a word every trader already owns a
definition of, and this is Praxis's, not a claim about the general one.

## Why not the alternatives

**One trade per entry order.** Each exposure-increasing order would open its own
trade, and exits would be allocated between them by FIFO, LIFO or pro rata. It
is the definition a trader would give, and it is rejected because it contradicts
the accounting: section 4 settles that partial closes use weighted average cost,
so the system genuinely does not know which entry an exit closed. Choosing a
policy would invent that knowledge, and two different policies would produce
different behavioural statistics from identical fills.

**A campaign the user declares.** The trader groups orders under an identifier
of their own. It captures intent, and it is rejected as a primary unit because
it depends on manual labelling, lets the analytical unit be changed after the
fact, breaks when a label is forgotten, and lets two people describe the same
events differently. It may return later as an analytical annotation. It may
never be the accounting truth.

## Fixed points

**Identity is deterministic and needs no counter.** An episode's identifier is
the journal sequence number of the `PositionChanged` event with kind `opened`
that began it. It is unique, reproducible on replay because replay assigns the
same sequences, and it points at the event that created it rather than at a
registry that has to be kept in step.

**A flip is two episodes at one logical time**, in an order the log already
makes explicit: the `closed` event precedes the `opened` one and carries a lower
sequence, so causality is recorded and not inferred.

**An episode's result is realised P&L minus the commissions of every one of its
legs.** Fees are part of the outcome, not an adjustment to it.

**Break-even includes commissions.** An episode that gave back its gain in fees
was not economically flat, and calling it flat would hide the most ordinary way
a session is lost.

**An open episode has no result.** It is not a winner, not a loser and not
break-even until it ends. Anything that classifies episodes must skip the open
one rather than treat it as zero.

**One instrument, one episode at a time.** The account is net, so a long and a
short episode in the same instrument cannot coexist. Separate instruments have
separate episodes.

**An order that was rejected, cancelled, or has not filled opens nothing.** An
episode begins at a fill.

**Identity and result are reconstructible without heuristics.** Both already
follow from events the journal records: the boundaries from `PositionChanged`
kinds `opened` and `closed`, the result from the `RealisedCts` and `FeeCts` of
the legs between them.

## A losing close is not a losing trade

`OrderContext.ConsecutiveLosses` counts **closing legs** that realised a loss.
It is honestly named and it is not what a hypothesis about losing streaks means:
scaling out of one bad position in two reductions would count as two losses,
when it is one trade being abandoned in pieces.

The conditioning event for a hypothesis about behaviour after a run of losses is
**two completed losing episodes**, which needs a second counter,
`ConsecutiveLosingTrades`, advanced only when an episode ends.

`ConsecutiveLosses` is **not renamed and not repurposed**. It means what it has
always meant, and changing that would silently change what every journal already
written says about its own past. The two counters coexist, measure different
things, and both are facts.

## Consequences

The unit for money and the unit for behaviour are not the same, and that is the
point: an episode has a P&L and no single decision behind it, while an entry is
a decision with no P&L of its own. Protective levels attach to the entry, so no
lot tracking appears anywhere. See
[`docs/experiment/trade-identity.md`](../experiment/trade-identity.md).

A trader who scales in and out across an afternoon has one large episode, and
"the size of the next entry" has no meaning inside it. That is a real limitation
of this definition, accepted because the alternative fabricates a relation the
system does not have.

Nothing here changes how money is accounted. No type, no event and no stored
figure is added by this decision alone — the identifier is derived and the
result is a sum. What follows from it is the design of the protection events and
one new counter, and those change the payload format.

## The format change this implies

`ConsecutiveLosingTrades` is a new field on `order_submitted`, and the
protection events are new lines. Both change `praxis.event.v1`, whose golden
bytes have already been changed once, for the source sequence.

This was first written as "they land together and that is the last one", which
was too narrow: resting orders needed their own events and landed before them.

The honest rule is an **epoch**, not a single change. `praxis.event.v1` is still
gaining event kinds while the kernel is unfinished — resting and cancellation,
then protection, then this counter — and it closes when the last of them lands.
After that, anything further is `v2` with the compatibility table the store
already carries. Stating the epoch is what keeps "the last change" from being
said a fourth time.

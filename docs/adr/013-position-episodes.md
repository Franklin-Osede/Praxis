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

## Binding protection to an episode

Protection is decided before the position exists, so `ProtectionPlaced` names
the entry's order. Binding it to an episode is unambiguous only when the fill
opens one from flat, and that is not the only case. An order carrying
protection may add to an episode that already exists; two entries may be
waiting when a third opens; an order that looked like an entry may end up
reducing or closing because of what happened before it; and a fill may flip.

**The binding follows the effect the fill actually had**, not what the order
appeared to be:

```text
opened   -> bind to the episode this fill created
added    -> bind to the episode already active
flip     -> bind to the new episode, never to the one it closed
reduced  -> the planned protection cannot activate
closed   -> the planned protection cannot activate
```

Requiring protection to be placed only from flat was rejected: it would block
an entry that adds to a position, which is exactly the behaviour worth
measuring. Keeping protection per entry through its whole life was rejected
because it needs exits allocated across entries, which is the lot tracking this
ADR already refused.

**Protection that cannot activate is cancelled, and the cancellation is
recorded.** A plan that quietly evaporates is the same defect as the vanishing
remainder: the log would hold a decision the engine never honoured.

### Fixed before implementation

- `ProtectionPlaced` with both levels at zero is invalid. It protects nothing
  and would be indistinguishable from an entry that placed none.
- Cancelling an entry that never executed ends its planned protection.
- A partial fill activates protection over the exposure actually opened, not
  over the quantity that was ordered.
- Later fills of the same entry extend the protected quantity. They do not
  create another episode and they do not create a second protection.
- Stop and target are one-cancels-the-other. Either executing ends the other.
- A partial execution reduces the sibling's quantity to what is left. Protection
  can never close contracts that no longer exist.
- Where both levels are reachable on one observation, the **stop** is evaluated
  first, as ADR-004 already settles for an ambiguous bar.
- Replacing a level from zero is placing it, not widening it. `Widened` compares
  against a level that existed.
- `Widened` is derived, so `Verify` and `Replay` recompute it from the levels
  either side of the change rather than believing the field.

### The state machine

```text
Planned   -> placed with an entry, before any fill
Active    -> the entry's fill opened or added to an episode
Replaced  -> levels changed while active
Cancelled -> withdrawn, or the entry never opened exposure
Executed  -> a level was reached and its sibling ended with it
```

## The format change this implies

`ConsecutiveLosingTrades` is a new field on `order_submitted`, and the
protection events are new lines.

An earlier draft said they would be the last change to `praxis.event.v1`. That
was wrong twice over: resting orders needed their own events and landed first,
and more importantly **a version is stable when its bytes stop changing, not
when anyone promises the next change will be the last**. A writer and a command
line can already produce v1 journals.

So v1 keeps the schema it has, and these facts inaugurate **`praxis.event.v2`**:

- New journals are written in v2.
- The compatibility table selects the codec, which is what it was built for.
- A v1 journal still reads, still proves and can still be appended to — in v1.
  Appending never rewrites a journal into a newer schema.
- A v1 journal honestly lacks the new facts rather than appearing to hold them,
  and writing one into it is refused rather than dropped.

Whether a v1 journal may be migrated so that a session can use protection is a
separate decision, and is not taken here.

This also exercises the version machinery now, while no journal holds anything
worth losing.

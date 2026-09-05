# ADR-014 — Protection is an aggregate, not two prices

Status: accepted and implemented. `praxis.event.v3` is published by the
commands that place, change and withdraw a protection; a plan becomes active
from the real effect of a fill; and `praxis.event.v4` is published by
one-cancels-the-other execution, which needed cancellation reasons no earlier
version has a name for.

## What this corrects

ADR-013 and `praxis.event.v2` treated a protective level as a pair of numbers
attached to something else. Three things the producer cannot honour followed
from that, and none of them is a detail:

**A planned protection cannot be modified or withdrawn.** `ProtectionPlaced`
names the entry's order, because at the moment of the decision no episode
exists. `ProtectionReplaced` and `ProtectionCancelled` name only an episode. So
a limit entry placed with a stop, whose stop is then moved before the entry
fills, cannot be written down — and neither can withdrawing the protection
while keeping the entry. `OrderCancelled` covers only the case where the entry
goes too.

**A protective fill has no order to belong to.** `market.Fill` requires an
`OrderID`, and the protection events create no order. Reusing the entry's
identifier would state that the order which opened a position also closed it,
which is false and would corrupt every count that groups fills by decision.

**Executed is not terminal.** A protection over ten contracts may execute three
for want of depth. The stop's remainder is cancelled, seven contracts remain
open, the episode continues, and the position now has a target and no stop.
That is not an ending.

Underneath all three: a protective level has an identity, a quantity and a life
of its own. It is a small aggregate, and calling it a field on something else
is what produced a schema its own producer cannot satisfy.

## Decision

### References are a tagged union

Protection is placed against an entry and lives on against an episode, so the
events that change it must be able to name either.

```go
type ProtectionRefKind uint8

const (
    ProtectionRefEntry ProtectionRefKind = iota + 1
    ProtectionRefEpisode
)

type ProtectionRef struct {
    Kind      ProtectionRefKind
    OrderID   string
    EpisodeID uint64
}
```

`EpisodeID == 0` is not used to mean "an entry instead". A zero that means
something is the same defect as a monetary zero that means "not configured",
which the drawdown accessors already had to be rescued from.

Forbidding changes while an entry waits was rejected. Moving a stop before the
entry fills is ordinary, and it is behaviour worth measuring: making the trader
cancel and resubmit the whole entry would erase the very decision the
experiment is about.

### Each leg is an order with a name, and no name is ever reused

`ProtectionPlaced` records `StopOrderID` and `TargetOrderID` explicitly, so a
fill, an OCO cancellation and a reconstruction all point at something that
exists rather than at a convention a reader has to know.

They are drawn from a **reserved namespace**, keyed by the sequence of the
`ProtectionPlaced` event that created them:

```text
praxis:<sequence>:stop
praxis:<sequence>:target
```

Deriving them from the entry's identifier was the first idea and is worse: it
depends on what the trader called the entry, and a trader can call an entry
anything. A sequence is globally unique by construction, owes nothing to the
entry's length or content, and reproduces on replay because replay assigns the
same sequences. An order whose identifier begins with `praxis:` is refused at
submission, so the namespace belongs to the system alone.

**No identifier is ever reused, in the whole journal.** Refusing collisions
only with orders currently working is not enough: an order that finished having
used a name leaves that name attributable, and a later order taking it would
make grouping fills by decision ambiguous again — which is the thing the
identifier exists for.

Replay therefore reconstructs the set of identifiers a journal has used, and
`Resume` carries it. A map is the right structure for that: it answers only
membership and is never iterated to produce a result, so rule 4 is not in
question.

### The state machine

```text
Planned                                  placed with an entry, before any fill
Active(stop?, target?, protectedQty)     the entry's fill opened or added
Ended                                    no exposure left, or both legs gone
```

`Executed` is a reason for a transition, never a state. While exposure remains,
an execution moves `Active` to another `Active`:

```text
target fills partly  -> Active(stop unchanged, target reduced, qty reduced)
stop fills partly    -> Active(no stop, target reduced, qty reduced)
either fills wholly  -> Ended, the sibling cancelled with it
position reaches flat-> Ended
both legs cancelled  -> Ended
```

The second line is the one that made this necessary: a stop that reached its
level has triggered and cannot untrigger (ADR-004), so its remainder is
cancelled — and the target survives over what is left.

### Activation reads the whole effect of a fill, not each change in turn

A reversal is one fill producing two changes: the position closes and the
opposite one opens. A projection reacting to the close alone would end the
arriving plan as having opened no exposure, one event before the exposure it
opens, and the plan could never be tied to the episode it was placed for.

A fill's changes are therefore assembled before any of them is recorded, and
the batch is written in causal order:

```text
FillProduced
PositionChanged   closed
ProtectionEnded   the old episode's protection, reason=flipped
PositionChanged   opened
                  the plan activates over the new episode, derived
```

Activation itself is **derived and not recorded**. There is no
`ProtectionActivated`: a plan binding to an episode is a consequence of a
position change that is already in the log, and an event saying so would be a
second copy of a fact — the kind that can disagree with the first.

Only an event ever removes a protection. Folding a change never does. That is
what lets `Verify`, which holds the events but not the fills, reach the same
state as `Replay`, which holds both: `Verify` cannot see whether a close was a
reversal or an exit, and that is the only thing the ending's reason turns on.
So `Verify` demands that a closed episode keeps nothing protecting it, and
`Replay` proves which ending was owed and why.

### The levels must be on the right sides of each other

A long whose stop sits above its target has the two legs doing each other's
job, and both can be reachable within a single observation — at which point
which one executes is decided by the order the engine happens to visit them in,
and the log records an outcome the trader could not have predicted from what
they placed. Equality is the same defect with no gap in it.

```text
long, both present    stop < target
short, both present   target < stop
either alone          nothing to be on the wrong side of
```

It does **not** require the entry's price to lie between them. A market order
can gap, and its fill is not known when the levels are decided. A level the
market has already passed is a question for activation, which knows what the
fill actually was; deciding it here would rewrite the trader's decision with
information they did not have.

### Only one protection governs an episode

An entry carrying protection may fill into an episode that already has some.
Two plans then exist for one position, and none of the obvious answers is
honest: keeping both is protection per lot, which ADR-013 rejected; silently
replacing the old one changes the whole episode without naming the change; and
ignoring the new one makes a recorded decision disappear.

The plan therefore activates or is explicitly ended, according to what the fill
did:

```text
opened                          activate over the new episode
added, episode unprotected      activate over the whole resulting episode
added, episode already protected  the new plan does not activate; it is ended
                                  with a stated reason, and the existing
                                  protection extends its quantity
flip                            end the old episode's protection; activate the
                                plan over the new episode
reduced or closed               end the plan: it opened no exposure
```

A trader who wants different levels on a position that already has them uses
`ReplaceProtection`. The change is then named as the decision it actually was,
rather than arriving disguised as an entry.

Ending a plan always carries a reason, and the reasons are facts about what the
system did rather than about why anyone did anything:

```go
type ProtectionEndReason uint8

const (
    ProtectionWithdrawnByTrader ProtectionEndReason = iota + 1
    ProtectionEntryCancelled
    ProtectionDidNotOpenExposure
    ProtectionAlreadyActive
    ProtectionPositionClosed
    ProtectionFlipped
    ProtectionExecuted
)
```

### A plan never outlives its entry

A protection planned against an order that no longer exists could never
activate, and a session that went on reporting it would be offering the trader
cover they do not have. The entry's cancellation and the ending of its plan are
therefore one decision and one batch, in that order:

```text
OrderCancelled   reason=by_trader | unfillable_remainder
ProtectionEnded  ref=entry, reason=entry_cancelled
```

`Replay` and `Verify` both demand it, and demand it of the position and not
merely of the totals: a log holding every ending it owes can still be false
about which cancellation each one answered. The ending is the very next event.

The rule asks whether the plan is still *planned* rather than assuming. Once a
plan has activated, the episode governs it and the entry disappearing means
nothing — a partly filled entry whose remainder is cancelled keeps protecting
what it opened.

### Once active, the episode governs

After the first fill activates a plan, the entry's reference stops governing
it. Further changes name the episode.

The entry's remainder may still be working, and its later fills **extend the
protected quantity and nothing else**. They do not reactivate the plan and they
do not restore the levels it was placed with. Without that rule a replacement
made after a partial fill would be silently undone by the next fill of the same
entry — the trader would see the levels they had already moved away from, put
back by an event they did not cause.

### Execution meets the observation that activated it

A protection is offered the same observation that activated it, not the next
one. An entry that filled through a gap may already be past its stop, and
waiting would grant a survival the market never gave.

Within one observation the order is fixed:

```text
protections already standing
each working order, in the order it was submitted
    its fills
    its protection, resolved before the next order is offered anything
revalue
```

The interleaving is the point. A working order can fill and activate a stop
this same observation has already passed; leaving every protection to the end
would let the next working order take the liquidity that stop should have
found. The stop goes first within a protection for the same reason: without a
real queue position nothing in the data says which of two reachable levels the
market took first, and a simulator that chose the target would hand the trader
the better of two outcomes it cannot know.

That priority has no test yet, because with a two-sided quote the situation
cannot be constructed: a long's stop needs a bid at or below its level and its
target a bid at or above a higher one. A property test pins that instead —
which is also what would break first if the geometry rule were relaxed, and
that is the failure that would let the priority start to matter. Bars will make
it constructible.

A leg is built fresh from the protection's state each time rather than held as
a working order. The quantity it covers is the episode's exposure and that
changes underneath it, so a leg kept as an order would have to be rewritten on
every fill, and the two copies would eventually disagree.

### Every leg ends with an event

```text
target fills 3 of 10     both legs stand over 7
stop fills 3 of 10       the stop's remainder of 7 is cancelled,
                         unfillable_remainder; the target stands over 7
stop through, book empty the whole 10 is cancelled, unfillable_remainder;
                         the position is untouched
either leg closes 10     the sibling is cancelled by_oco, the aggregate ends
                         executed
a manual exit closes 10  both legs are cancelled position_closed, the
                         aggregate ends position_closed
```

`by_oco` and `position_closed` both end a leg, and the observable cause is not
the same: a log that spelled them alike could not tell a stop that worked from
one the trader overtook. They are values a v3 reader has no name for, so they
inaugurate **`praxis.event.v4`**, published with the commands that first write
them — the rule v3 was corrected into. A v3 writer refuses them and a v3 reader
refuses to guess at them.

A stop that reached its level and found nothing has still triggered, and there
is no position change to derive that from: its cancellation is recorded and
believed. Everything else in a protective tail is recomputed — which order,
how much was left on it, why, and what the protection was holding when it
ended.

### Fills that are not protective adjust it too

- A manual partial exit reduces the protected quantity.
- A manual full exit ends the protection and cancels both legs.
- An addition extends the protected quantity, because protection binds to the
  episode and the episode grew. It does not create a second protection.
- A flip ends the old episode's protection before the new episode's can begin.
  Protection never crosses a flip: ADR-013 already settles that the two sides
  are different trades.

Protection can never close contracts that no longer exist. Every adjustment
reduces toward the exposure that is actually there.

## When v3 gets its bytes

The protection events in `praxis.event.v2` were defined ahead of any producer,
on the argument that a schema is defined and its producer catches up. That
argument was mine and it was wrong here: **a schema is not proven until
something both produces and consumes it**, and the three holes above are
exactly what a producer would have found on the first attempt.

So `praxis.event.v3` was designed here and published with the first commands
that write it.

The protection lines declared in v2 were removed rather than kept readable. A
published version's bytes are protected because journals hold them, and no
journal ever held those: nothing wrote them and, their shape being wrong,
nothing could. What v2 does produce — a decision carrying a losing-trade streak
— is untouched, and v1 is untouched byte for byte. Keeping an unwritable schema
to honour a rule about written data would be the rule cargo-culted rather than
applied.

v1 and v2 both stay readable. The compatibility table already selects the
codec, and this is the second time it has earned its place.

## Scenarios that must pass before this is believed

Each is a transition the machine has to name, and every one of them ends with a
replay and a resume:

1. An entry that never executes, cancelled — its planned protection ends. ✓
2. An entry that never executes, left waiting — the protection is still
   planned, and a later fill activates it. ✓
3. A stop moved while the entry is still waiting. ✓
4. Protection withdrawn while the entry keeps waiting. ✓
5. A partial entry fill — protection covers what opened, not what was ordered. ✓
6. A later fill of the same entry — the protected quantity grows, no second
   protection appears. ✓
7. An addition from a different entry — the protected quantity grows. ✓
8. A manual partial exit — the protected quantity shrinks. ✓
9. A manual full exit — the protection ends with the position. ✓
10. A target filling wholly — the stop is cancelled with it. ✓
11. A stop filling partly for want of depth — its remainder is cancelled, the
    target survives over what is left, the episode stays open. ✓
12. A stop reaching its level with no liquidity at all — the whole leg is
    cancelled, the target survives. ✓
13. A flip — the old protection ends before the new episode's begins. ✓
14. A resume in each of `Planned`, `Active` with both legs, `Active` with one,
    and after `Ended`. ✓
15. An entry filling into an episode that is already protected — the new plan
    ends with `ProtectionAlreadyActive` and the existing quantity grows. ✓
16. A partial entry fill, then a replacement by episode, then the entry's
    remainder filling — the replaced levels stand and only the quantity grows. ✓
17. An identifier reused after its order finished — refused. ✓
18. An order submitted under the `praxis:` namespace — refused. ✓

## Consequences

Protection is the first thing in Praxis with a lifecycle that is neither an
order nor a position, and it will need its own projection alongside the episode
one — derived from events, shared by the live session, `Verify` and `Replay`,
for the same reason: a second implementation would eventually disagree, and the
disagreement would be between a journal and the thing that checks it.

The implementation splits along the machine rather than along the packages:
planned protection and its commands with no execution; activation and binding by
the fill's real effect; one-cancels-the-other execution with partial
quantities; then verification, replay, resume and persistence; and only then a
human command.

The first of those is small and its exit criterion is small: a planned
protection can be placed, changed, withdrawn, persisted and reconstructed
exactly, while its entry is still waiting. Nothing executes a level yet.

Two things belonged to the join between that slice and the next, and were done
before activation rather than with it: the ending of a plan whose entry was
cancelled, and the ordering that puts `ProtectionPlaced` before the fill.
Neither is about activation, and both are what activation had to stand on.

Every scenario above now passes. What remains is not protection: it is a human
command, and then the pilot sessions the experiment is actually for.

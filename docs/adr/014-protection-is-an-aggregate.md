# ADR-014 — Protection is an aggregate, not two prices

Status: accepted. Its schema is designed here and is **not** published yet; see
"When v3 gets its bytes".

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

### Each leg is an order with a name

`ProtectionPlaced` records `StopOrderID` and `TargetOrderID`. They are derived
from the entry's identifier — `<entry>:stop` and `<entry>:target` — so they are
reproducible on replay, and they are recorded explicitly so that a fill, an
OCO cancellation and a reconstruction all point at something that exists rather
than at a convention a reader has to know.

The session refuses any order whose identifier is already in use by a working
order or a protective leg, so a trader cannot name an order into a collision.

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

So `praxis.event.v3` is designed here and published with the commands that
write it, not before. Until then v2 stays as it is, and its protection lines
remain what they are — a schema nothing ever wrote, kept readable because that
is what a published version means.

v1 and v2 both stay readable. The compatibility table already selects the
codec, and this is the second time it has earned its place.

## Scenarios that must pass before this is believed

Each is a transition the machine has to name, and every one of them ends with a
replay and a resume:

1. An entry that never executes, cancelled — its planned protection ends.
2. An entry that never executes, left waiting — the protection is still
   planned, and a later fill activates it.
3. A stop moved while the entry is still waiting.
4. Protection withdrawn while the entry keeps waiting.
5. A partial entry fill — protection covers what opened, not what was ordered.
6. A later fill of the same entry — the protected quantity grows, no second
   protection appears.
7. An addition from a different entry — the protected quantity grows.
8. A manual partial exit — the protected quantity shrinks.
9. A manual full exit — both legs are cancelled.
10. A target filling wholly — the stop is cancelled with it.
11. A stop filling partly for want of depth — its remainder is cancelled, the
    target survives over what is left, the episode stays open.
12. A stop reaching its level with no liquidity at all — the whole leg is
    cancelled, the target survives.
13. A flip — the old protection ends before the new episode's begins.
14. A resume in each of `Planned`, `Active` with both legs, `Active` with one,
    and after `Ended`.

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

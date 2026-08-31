package session

import (
	"errors"
	"fmt"

	"praxis/internal/market"
)

// ErrOutOfOrder reports an append that would break the journal's one ordering.
var ErrOutOfOrder = errors.New("session: event is not after the last appended one")

// Journal is an append-only, ordered record of everything a session did.
//
// It is deliberately a concrete type and not yet an interface. EventStorePort
// is exempted from the no-premature-interface rule, but the exemption permits
// the boundary; it does not require inventing it before a file-backed store
// exists to sit behind it.
type Journal struct {
	events   []Event
	lastTime market.LogicalTime
	lastSeq  uint64
	started  bool
}

// Append adds an event, rejecting one that is not strictly after the last.
// Ordering is enforced rather than sorted, because a stream that has to be
// sorted has already lost the information that says it was wrong.
func (j *Journal) Append(e Event) error {
	h := e.Header()
	if j.started && !(h.Time > j.lastTime || (h.Time == j.lastTime && h.Sequence > j.lastSeq)) {
		return fmt.Errorf("%w: (%d, %d) after (%d, %d)", ErrOutOfOrder, h.Time, h.Sequence, j.lastTime, j.lastSeq)
	}
	j.events = append(j.events, e)
	j.lastTime, j.lastSeq, j.started = h.Time, h.Sequence, true
	return nil
}

// Events returns the recorded events in order.
func (j *Journal) Events() []Event {
	out := make([]Event, len(j.events))
	copy(out, j.events)
	return out
}

func (j *Journal) Len() int { return len(j.events) }

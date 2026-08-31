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

// ValidateNext reports whether a position would be accepted, without changing
// anything.
//
// A command must ask this before it lets any aggregate decide, because those
// decisions mutate: an evaluation that accepted a session boundary the journal
// then refused would leave the challenge in one session and the log in
// another, with nothing to reconcile them.
func (j *Journal) ValidateNext(at market.LogicalTime, sequence uint64) error {
	if !j.started || at > j.lastTime || (at == j.lastTime && sequence > j.lastSeq) {
		return nil
	}
	return fmt.Errorf("%w: (%d, %d) after (%d, %d)", ErrOutOfOrder, at, sequence, j.lastTime, j.lastSeq)
}

// Append adds an event, rejecting one that is not strictly after the last.
// Ordering is enforced rather than sorted, because a stream that has to be
// sorted has already lost the information that says it was wrong.
func (j *Journal) Append(e Event) error {
	h := e.Header()
	if err := j.ValidateNext(h.Time, h.Sequence); err != nil {
		return err
	}
	j.events = append(j.events, e)
	j.lastTime, j.lastSeq, j.started = h.Time, h.Sequence, true
	return nil
}

// newJournalFrom rebuilds a journal from events already recorded, so a resumed
// session continues one ordering rather than starting a second.
func newJournalFrom(events []Event) (*Journal, error) {
	j := &Journal{}
	for _, e := range events {
		if err := j.Append(e); err != nil {
			return nil, err
		}
	}
	return j, nil
}

// Events returns the recorded events in order.
func (j *Journal) Events() []Event {
	out := make([]Event, len(j.events))
	copy(out, j.events)
	return out
}

func (j *Journal) Len() int { return len(j.events) }

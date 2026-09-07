package session

import (
	"errors"
	"fmt"
)

// Errors reported when a presentation cannot be recorded, or a decision arrives
// without one.
var (
	// ErrNothingToPresent reports an acknowledgement for an observation that
	// has not happened. There is nothing on the screen to confirm.
	ErrNothingToPresent = errors.New("session: no observation is waiting to be presented")

	// ErrWrongPresentation reports an acknowledgement naming an observation or
	// a segment that is not the one waiting. A stale tab confirming what it
	// last saw must not stand in for the confirmation of what is on screen now.
	ErrWrongPresentation = errors.New("session: that is not the presentation waiting to be confirmed")

	// ErrNotYetPresented reports a human command taken before the interface
	// confirmed the person could see what they were acting on. Accepting it
	// would record a decision with no measurable beginning, and the interval
	// from presentation to decision is the quantity the whole clock exists for.
	ErrNotYetPresented = errors.New("session: nothing has been confirmed as presented in this segment")
)

// PresentationID names one showing of one observation.
//
// It is the segment and the observation together, and it is derived rather than
// minted: the same quote shown again after a reload is a different presentation,
// because a reload is a new run of interaction and the elapsed reading it will
// be subtracted from starts over.
type PresentationID struct {
	Segment          uint64
	ObservedSequence uint64
}

func (p PresentationID) String() string {
	return fmt.Sprintf("%d:%d", p.Segment, p.ObservedSequence)
}

// Pending is the presentation waiting to be confirmed, and whether there is
// one. A session that has seen no observation has nothing to confirm.
func (s *Session) Pending(segment uint64) (PresentationID, bool) {
	if s.lastObserved == 0 {
		return PresentationID{}, false
	}
	id := PresentationID{Segment: segment, ObservedSequence: s.lastObserved}
	if s.presented == id {
		return PresentationID{}, false
	}
	return id, true
}

// Presented reports whether this segment has confirmed what is on the screen.
// A command taken before that has no beginning to be measured from.
func (s *Session) Presented(segment uint64) bool {
	return s.presented.Segment == segment && s.presented.ObservedSequence == s.lastObserved
}

// AcknowledgePresentation records that the interface put an observation in
// front of the person and said so.
//
// A repeated acknowledgement of what is already confirmed adds nothing: a lost
// response is a retry, and a second event would claim the screen was drawn
// twice. One naming anything else is refused, because a stale tab confirming
// what it last saw must not stand in for the confirmation of what is on screen
// now.
func (s *Session) AcknowledgePresentation(id PresentationID, at Instant) error {
	return s.command(func() error { return s.acknowledgePresentation(id, at) })
}

func (s *Session) acknowledgePresentation(id PresentationID, at Instant) error {
	if at.Malformed() || at.IsZero() {
		return fmt.Errorf("%w: %+v", ErrMalformedDecision, at)
	}
	if id.Segment != at.Segment {
		return fmt.Errorf("%w: %s confirmed in segment %d", ErrWrongPresentation, id, at.Segment)
	}
	if s.presented == id {
		// Already confirmed. Nothing is recorded, which is what makes a retry
		// free of consequence.
		return nil
	}
	pending, waiting := s.Pending(id.Segment)
	if !waiting {
		return fmt.Errorf("%w: %s", ErrNothingToPresent, id)
	}
	if pending != id {
		return fmt.Errorf("%w: %s is waiting, not %s", ErrWrongPresentation, pending, id)
	}

	when := s.lastQuote.Time
	if err := s.journal.ValidateNext(when, s.sequence+1); err != nil {
		return err
	}
	if err := s.record(when, KindObservationPresented, func(e Envelope) Event {
		return ObservationPresented{
			Envelope: e, ObservedSequence: id.ObservedSequence, Presented: at,
		}
	}); err != nil {
		return err
	}
	s.presented = id
	return nil
}

// requirePresented refuses a human command taken before the interface confirmed
// the person could see what they were acting on.
//
// A scripted run has no interface and no segment, so it is not held to this: a
// decision with no segment is nobody's, and nobody needs to have been shown
// anything.
func (s *Session) requirePresented(d Decision) error {
	if d.IsZero() {
		return nil
	}
	if !s.Presented(d.Segment) {
		return fmt.Errorf("%w: segment %d has not confirmed observation %d",
			ErrNotYetPresented, d.Segment, s.lastObserved)
	}
	return nil
}

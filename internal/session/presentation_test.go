package session_test

import (
	"errors"
	"reflect"
	"testing"

	"praxis/internal/market"
	"praxis/internal/session"
)

func instant(segment uint64, elapsed session.ElapsedNanos) session.Instant {
	return session.Instant{
		AtUTCNanos:   session.UnixNanos(1_764_000_000_000_000_000 + int64(elapsed)),
		Segment:      segment,
		ElapsedNanos: elapsed,
	}
}

// Scenario: a decision taken before anything was confirmed as shown is refused
//
//	Given an observation the interface has not acknowledged
//	When a command arrives
//	Then it is refused, and nothing is recorded.
//
// The interval from presentation to decision is what the whole clock exists to
// make measurable. A command with no confirmed beginning is a decision whose
// latency cannot be computed and whose stimulus nobody can say arrived.
func TestACommandBeforeTheScreenWasConfirmedIsRefused(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	if err := s.Observe(sized(3_000, 20_000, 20_001, 50), 1); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	before := s.JournalLen()

	if err := s.SubmitOrder(order("o1", market.SideBuy, 1), decidedAt()); !errors.Is(err, session.ErrNotYetPresented) {
		t.Fatalf("error: got %v, want %v", err, session.ErrNotYetPresented)
	}
	if s.JournalLen() != before || s.NeedsRecovery() != nil {
		t.Fatal("a refused command recorded something, or stopped the session")
	}

	// Confirmed, and now it is accepted.
	mustPresent(t, s)
	mustSubmit(t, s, order("o1", market.SideBuy, 1))
	checked(t, s)
}

// Scenario: a confirmation names the presentation it confirms
//
// A stale tab confirming what it last saw must not stand in for the
// confirmation of what is on the screen now, and a second segment showing the
// same quote is a different showing: a reload is a new run of interaction and
// the reading an interval is computed from starts over.
func TestAConfirmationNamesWhatItConfirms(t *testing.T) {
	build := func(t *testing.T) (*session.Session, session.PresentationID) {
		t.Helper()
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		if err := s.Observe(sized(3_000, 20_000, 20_001, 50), 1); err != nil {
			t.Fatalf("Observe: %v", err)
		}
		id, waiting := s.Pending(presentedIn)
		if !waiting {
			t.Fatal("nothing is waiting to be confirmed")
		}
		return s, id
	}

	t.Run("another observation", func(t *testing.T) {
		s, id := build(t)
		other := session.PresentationID{Segment: id.Segment, ObservedSequence: id.ObservedSequence + 1}
		if err := s.AcknowledgePresentation(other, instant(presentedIn, 0)); !errors.Is(err, session.ErrWrongPresentation) {
			t.Fatalf("error: got %v, want %v", err, session.ErrWrongPresentation)
		}
	})

	t.Run("another segment", func(t *testing.T) {
		s, id := build(t)
		other := session.PresentationID{Segment: 2, ObservedSequence: id.ObservedSequence}
		if err := s.AcknowledgePresentation(other, instant(presentedIn, 0)); !errors.Is(err, session.ErrWrongPresentation) {
			t.Fatalf("error: got %v, want %v", err, session.ErrWrongPresentation)
		}
		// And the confirmation's own moment must belong to the segment it
		// names, or the interval it begins is measured on another clock.
		if err := s.AcknowledgePresentation(id, instant(2, 0)); !errors.Is(err, session.ErrWrongPresentation) {
			t.Fatalf("error: got %v, want %v", err, session.ErrWrongPresentation)
		}
	})

	t.Run("nothing on the screen", func(t *testing.T) {
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		if _, waiting := s.Pending(presentedIn); waiting {
			t.Fatal("something is waiting before any observation")
		}
		id := session.PresentationID{Segment: presentedIn, ObservedSequence: 1}
		if err := s.AcknowledgePresentation(id, instant(presentedIn, 0)); !errors.Is(err, session.ErrNothingToPresent) {
			t.Fatalf("error: got %v, want %v", err, session.ErrNothingToPresent)
		}
	})
}

// Scenario: confirming twice records once
//
// A lost response is a retry. A second event would claim the screen was drawn
// twice and would give the same observation two beginnings.
func TestConfirmingTwiceRecordsOnce(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	if err := s.Observe(sized(3_000, 20_000, 20_001, 50), 1); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	id, _ := s.Pending(presentedIn)

	if err := s.AcknowledgePresentation(id, instant(presentedIn, 5)); err != nil {
		t.Fatalf("AcknowledgePresentation: %v", err)
	}
	after := s.JournalLen()

	// The retry, at a later instant, adds nothing.
	if err := s.AcknowledgePresentation(id, instant(presentedIn, 900)); err != nil {
		t.Fatalf("a retry was refused: %v", err)
	}
	if s.JournalLen() != after {
		t.Fatal("a repeated confirmation produced a second event")
	}
	if _, waiting := s.Pending(presentedIn); waiting {
		t.Fatal("something is still waiting after it was confirmed")
	}

	// The recorded moment is the first one, which is the one that began the
	// interval a decision will be measured from.
	var presented session.ObservationPresented
	for _, e := range s.Events() {
		if v, ok := e.(session.ObservationPresented); ok {
			presented = v
		}
	}
	if presented.Presented.ElapsedNanos != 5 {
		t.Fatalf("the retry replaced the moment: got %d, want 5", presented.Presented.ElapsedNanos)
	}
	checked(t, s)
}

// Scenario: the next observation has to be confirmed before it can be acted on
//
// The confirmation names one observation. A new quote is a new showing, and a
// command against it before the interface says it is on the screen has no
// beginning again.
func TestEachObservationIsConfirmedOnItsOwn(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	mustSubmit(t, s, order("o1", market.SideBuy, 1))

	if err := s.Observe(sized(4_000, 20_010, 20_011, 50), 2); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if err := s.SubmitOrder(order("o2", market.SideBuy, 1), decidedAt()); !errors.Is(err, session.ErrNotYetPresented) {
		t.Fatalf("error: got %v, want %v", err, session.ErrNotYetPresented)
	}
	mustPresent(t, s)
	mustSubmit(t, s, order("o2", market.SideBuy, 1))
	checked(t, s)
}

// Scenario: a presentation survives reconstruction, and a new segment does not
// inherit it
//
// Reconstruction is faithful or it is nothing. What a new run of interaction
// must not inherit is the confirmation: a reload has seen nothing yet, and its
// elapsed reading starts over.
func TestAPresentationReconstructsAndDoesNotCrossASegment(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))

	state, err := session.Replay(s.Events())
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	resumed, err := session.Resume(state, nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !resumed.Presented(presentedIn) {
		t.Fatal("a resumed session forgot what had been confirmed")
	}
	if _, waiting := resumed.Pending(presentedIn); waiting {
		t.Fatal("a resumed session wants what it already has confirmed")
	}

	// A new run of interaction has seen nothing.
	if resumed.Presented(2) {
		t.Fatal("a new segment inherited a confirmation made in another")
	}
	id, waiting := resumed.Pending(2)
	if !waiting || id.Segment != 2 {
		t.Fatalf("segment 2 has nothing to confirm: %+v", id)
	}

	// And the same quote confirmed again in the new segment is a second
	// presentation, because it is a second showing.
	before := resumed.JournalLen()
	if err := resumed.AcknowledgePresentation(id, instant(2, 0)); err != nil {
		t.Fatalf("AcknowledgePresentation: %v", err)
	}
	if resumed.JournalLen() != before+1 {
		t.Fatal("a new segment's confirmation recorded nothing")
	}
	checked(t, resumed)
}

// Property: a decision and the presentation it answers share a segment, and the
// decision is not earlier.
func TestPropertyADecisionFollowsThePresentationItAnswers(t *testing.T) {
	s := script(t)

	var (
		presented   = map[uint64]session.ElapsedNanos{}
		compared    int
		lastSegment uint64
	)
	for _, e := range s.Events() {
		switch v := e.(type) {
		case session.ObservationPresented:
			presented[v.Presented.Segment] = v.Presented.ElapsedNanos
			lastSegment = v.Presented.Segment
		case session.OrderSubmitted:
			if v.Decided.IsZero() {
				continue
			}
			began, ok := presented[v.Decided.Segment]
			if !ok {
				t.Fatalf("a decision in segment %d answers a presentation that is not there", v.Decided.Segment)
			}
			if v.Decided.ElapsedNanos < began {
				t.Fatalf("a decision at %dns answers a presentation at %dns", v.Decided.ElapsedNanos, began)
			}
			compared++
		}
	}
	if compared == 0 || lastSegment == 0 {
		t.Fatal("the script produced no decision to compare")
	}
	if !reflect.DeepEqual(s.Events(), script(t).Events()) {
		t.Fatal("the script is not deterministic")
	}
}

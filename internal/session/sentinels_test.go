package session_test

import (
	"errors"
	"testing"

	"praxis/internal/market"
	"praxis/internal/session"
)

// Scenario: a reader's refusal says which rule refused it
//
//	Given a journal refused for one of the reasons a person can cause
//	Then errors.Is finds both the reader's sentinel and the rule's.
//
// The readers wrapped the cause with %v, so every refusal arrived as
// ErrStructure or ErrContradictoryLog and nothing else. That was survivable
// while the only caller was a CLI printing the text. It stops being survivable
// at the first handler: an interface has to tell a participant whether their
// stamp went backwards or whether they acted before the observation was
// confirmed, and those are two different things to say. Reading the sentence to
// find out is a strings.Contains in a handler, which is a parser for a message
// nobody promised to keep.
func TestARefusalNamesTheRuleThatRefusedIt(t *testing.T) {
	// A decision whose reading goes backwards inside its segment.
	backwards := func(t *testing.T) []session.Event {
		t.Helper()
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
		mustSubmit(t, s, order("o-1", market.SideBuy, 1))
		events := s.Events()
		at := indexOfKind(t, events, session.KindOrderSubmitted, 1)
		v := events[at].(session.OrderSubmitted)
		v.Decided.ElapsedNanos = 1 // behind the presentation that began it
		events[at] = v
		return events
	}

	// A journal somebody traded, with a command nobody timed.
	unstamped := func(t *testing.T) []session.Event {
		t.Helper()
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
		mustSubmit(t, s, order("o-1", market.SideBuy, 1))
		events := s.Events()
		at := indexOfKind(t, events, session.KindOrderSubmitted, 1)
		v := events[at].(session.OrderSubmitted)
		v.Decided = session.Decision{}
		events[at] = v
		return events
	}

	// A stamp that is half written.
	halfWritten := func(t *testing.T) []session.Event {
		t.Helper()
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
		mustSubmit(t, s, order("o-1", market.SideBuy, 1))
		events := s.Events()
		at := indexOfKind(t, events, session.KindOrderSubmitted, 1)
		v := events[at].(session.OrderSubmitted)
		v.Decided = session.Decision{GestureID: "g-half"}
		events[at] = v
		return events
	}

	for _, tc := range []struct {
		name  string
		build func(*testing.T) []session.Event
		rule  error
	}{
		{"a reading that went backwards", backwards, session.ErrInteractionOrder},
		{"a command nobody timed", unstamped, session.ErrInteractionStamp},
		{"a stamp half written", halfWritten, session.ErrMalformedDecision},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := tc.build(t)

			_, replayErr := session.Replay(events)
			if replayErr == nil {
				t.Fatal("Replay accepted it")
			}
			if !errors.Is(replayErr, session.ErrStructure) {
				t.Fatalf("Replay: %v does not carry ErrStructure", replayErr)
			}
			if !errors.Is(replayErr, tc.rule) {
				t.Fatalf("Replay: %v does not carry %v, so a caller cannot tell which rule refused it",
					replayErr, tc.rule)
			}

			verifyErr := session.Verify(events)
			if verifyErr == nil {
				t.Fatal("Verify accepted it")
			}
			if !errors.Is(verifyErr, session.ErrContradictoryLog) {
				t.Fatalf("Verify: %v does not carry ErrContradictoryLog", verifyErr)
			}
			if !errors.Is(verifyErr, tc.rule) {
				t.Fatalf("Verify: %v does not carry %v", verifyErr, tc.rule)
			}
		})
	}

	// And the two rules stay apart: one refusal is not the other.
	t.Run("the rules are not interchangeable", func(t *testing.T) {
		_, err := session.Replay(backwards(t))
		if errors.Is(err, session.ErrInteractionStamp) {
			t.Fatalf("a backwards reading also reports a stamp disagreement: %v", err)
		}
	})
}

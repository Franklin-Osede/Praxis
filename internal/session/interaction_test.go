package session_test

import (
	"errors"
	"strings"
	"testing"

	"praxis/internal/challenge"
	"praxis/internal/market"
	"praxis/internal/session"
)

// scriptedSession is a run nobody traded: no subject, no pacing, and therefore
// nothing that may carry a person's clock.
func scriptedSession(t *testing.T) *session.Session {
	t.Helper()
	gestures, acknowledgements, elapsedNow = 0, 0, 0
	cfg := config()
	cfg.SubjectID, cfg.Pacing = "", session.PacingScripted
	s, err := session.New(cfg, 1_000, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.OpenTradingSession(2_000, challenge.SessionID("d1")); err != nil {
		t.Fatalf("OpenTradingSession: %v", err)
	}
	if err := s.Observe(sized(3_000, 20_000, 20_001, 50), 1); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	return s
}

func stamp(segment uint64, elapsed session.ElapsedNanos) session.Instant {
	return session.Instant{
		AtUTCNanos:   session.UnixNanos(1_764_000_000_000_000_000 + int64(elapsed)),
		Segment:      segment,
		ElapsedNanos: elapsed,
	}
}

func gesture(id string, segment uint64, elapsed session.ElapsedNanos) session.Decision {
	at := stamp(segment, elapsed)
	return session.Decision{
		GestureID: id, AtUTCNanos: at.AtUTCNanos,
		Segment: at.Segment, ElapsedNanos: at.ElapsedNanos,
	}
}

// Scenario: a run nobody traded records nobody acting in it
//
//	Given a scripted session — no subject, no interface, no person
//	When an observation is acknowledged as presented
//	Then it is refused, because that event does not belong to a run nobody
//	  watched. A zero stamp on it would not be honest either: it is not that
//	  nobody confirmed the presentation, it is that nothing was presented.
func TestAScriptedRunRecordsNoPresentation(t *testing.T) {
	s := scriptedSession(t)
	before := s.JournalLen()

	// Nothing is even offered: a run nobody watched has nothing on a screen.
	if _, waiting := s.Pending(7); waiting {
		t.Fatal("a scripted run offered a presentation to confirm")
	}
	// And one built by hand is refused rather than quietly accepted.
	id := session.PresentationID{Segment: 7, ObservedSequence: observedSequence(t, s)}
	if err := s.AcknowledgePresentation(id, stamp(7, 500)); !errors.Is(err, session.ErrInteractionStamp) {
		t.Fatalf("AcknowledgePresentation: got %v, want %v", err, session.ErrInteractionStamp)
	}
	if s.JournalLen() != before {
		t.Fatalf("journal: got %d events, want the %d it had", s.JournalLen(), before)
	}
	if err := s.NeedsRecovery(); err != nil {
		t.Fatalf("a refusal made the session unusable: %v", err)
	}
}

// Scenario: a presentation belongs to the same chronology as the decisions
//
//	Given a person acting in one segment
//	Then the confirmation that put an observation on their screen is held to
//	  the same order as the decisions it will be subtracted from: its segment
//	  never goes back, and it never sits later in a segment than a decision
//	  already recorded there.
//
// The second is the one that matters. Latency is decision.Elapsed minus
// presentation.Elapsed, and until this the journal could hold a presentation at
// 9000ns and the decision answering it at 10ns — a negative reading of the one
// quantity the clock exists to produce.
func TestAPresentationSharesTheChronologyOfTheDecisions(t *testing.T) {
	t.Run("its segment never goes backwards", func(t *testing.T) {
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		if err := s.Observe(sized(3_000, 20_000, 20_001, 50), 1); err != nil {
			t.Fatalf("Observe: %v", err)
		}
		id5, _ := s.Pending(5)
		if err := s.AcknowledgePresentation(id5, stamp(5, 1_000)); err != nil {
			t.Fatalf("AcknowledgePresentation in segment 5: %v", err)
		}
		before := s.JournalLen()

		id2, waiting := s.Pending(2)
		if !waiting {
			t.Fatal("nothing was waiting for segment 2")
		}
		if err := s.AcknowledgePresentation(id2, stamp(2, 50)); !errors.Is(err, session.ErrInteractionOrder) {
			t.Fatalf("AcknowledgePresentation in segment 2: got %v, want %v", err, session.ErrInteractionOrder)
		}
		if s.JournalLen() != before {
			t.Fatalf("journal: got %d events, want the %d it had", s.JournalLen(), before)
		}
	})

	t.Run("a decision never precedes the presentation it answers", func(t *testing.T) {
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		if err := s.Observe(sized(3_000, 20_000, 20_001, 50), 1); err != nil {
			t.Fatalf("Observe: %v", err)
		}
		id, _ := s.Pending(1)
		if err := s.AcknowledgePresentation(id, stamp(1, 9_000)); err != nil {
			t.Fatalf("AcknowledgePresentation: %v", err)
		}
		before := s.JournalLen()

		err := s.SubmitOrder(order("o1", market.SideBuy, 1), gesture("g-early", 1, 10))
		if !errors.Is(err, session.ErrInteractionOrder) {
			t.Fatalf("SubmitOrder: got %v, want %v", err, session.ErrInteractionOrder)
		}
		if s.JournalLen() != before {
			t.Fatalf("journal: got %d events, want the %d it had", s.JournalLen(), before)
		}
	})
}

// Scenario: a traded run stamps every command, and a scripted one stamps none
//
//	Given a session somebody traded
//	When a human command arrives carrying no moment at which anyone decided it
//	Then it is refused at the door rather than committed and refused by its own
//	  readers afterwards — which is a journal clean on disk and invalid to
//	  every reader of it, with no repair that can help.
func TestACommandsStampMustMatchTheRun(t *testing.T) {
	t.Run("a traded run refuses an unstamped command", func(t *testing.T) {
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
		before := s.JournalLen()

		if err := s.SubmitOrder(order("o1", market.SideBuy, 1), session.Decision{}); !errors.Is(err, session.ErrInteractionStamp) {
			t.Fatalf("SubmitOrder: got %v, want %v", err, session.ErrInteractionStamp)
		}
		if s.JournalLen() != before {
			t.Fatalf("journal: got %d events, want the %d it had", s.JournalLen(), before)
		}
		if err := s.NeedsRecovery(); err != nil {
			t.Fatalf("a refusal made the session unusable: %v", err)
		}
	})

	t.Run("a scripted run refuses a stamped command", func(t *testing.T) {
		s := scriptedSession(t)
		before := s.JournalLen()

		err := s.SubmitOrder(order("o1", market.SideBuy, 1), gesture("g-1", 1, 10))
		if !errors.Is(err, session.ErrInteractionStamp) {
			t.Fatalf("SubmitOrder: got %v, want %v", err, session.ErrInteractionStamp)
		}
		if s.JournalLen() != before {
			t.Fatalf("journal: got %d events, want the %d it had", s.JournalLen(), before)
		}
	})

	t.Run("a cancellation is a decision too", func(t *testing.T) {
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
		resting, err := market.NewLimitOrder("o1", mnq, market.SideBuy, 1, 19_000)
		if err != nil {
			t.Fatal(err)
		}
		mustSubmit(t, s, resting)
		before := s.JournalLen()

		if err := s.CancelOrder("o1", session.Decision{}); !errors.Is(err, session.ErrInteractionStamp) {
			t.Fatalf("CancelOrder: got %v, want %v", err, session.ErrInteractionStamp)
		}
		if s.JournalLen() != before {
			t.Fatalf("journal: got %d events, want the %d it had", s.JournalLen(), before)
		}
	})
}

// Scenario: a refused interaction costs nothing
//
//	Given a command refused for any of the reasons above
//	Then the journal, the chronology and the register of acts are exactly as
//	  they were, and the same gesture may be sent again.
//
// This is what makes the check pure. The others prove the refusal happens; this
// proves it left nothing behind — which is the difference between validating
// and quietly mutating.
func TestARefusedInteractionSpendsNothing(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	if err := s.SubmitOrder(order("o1", market.SideBuy, 1), gesture("g-first", 1, 90_000_000)); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
	before := s.JournalLen()

	// Refused: the segment's reading went backwards.
	stale := gesture("g-retried", 1, 10_000)
	if err := s.SubmitOrder(order("o2", market.SideBuy, 1), stale); !errors.Is(err, session.ErrInteractionOrder) {
		t.Fatalf("SubmitOrder: got %v, want %v", err, session.ErrInteractionOrder)
	}
	if s.JournalLen() != before {
		t.Fatalf("journal: got %d events, want the %d it had", s.JournalLen(), before)
	}

	// The gesture was not spent, and the clock did not move: the same act,
	// corrected, is accepted.
	if err := s.SubmitOrder(order("o2", market.SideBuy, 1), gesture("g-retried", 1, 95_000_000)); err != nil {
		t.Fatalf("the corrected command was refused: %v", err)
	}
	checked(t, s)
}

// Scenario: acknowledging the same presentation twice is free
//
//	Given a presentation already confirmed, and a decision taken after it
//	When the interface retries the confirmation — a lost response, a slow
//	  network — carrying the stamp it originally sent
//	Then nothing is recorded and nothing is refused. A benign retry must not
//	  become an error the participant sees, and it must not move the clock past
//	  the decision that already followed it.
func TestAnAcknowledgementRetryIsFree(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	if err := s.Observe(sized(3_000, 20_000, 20_001, 50), 1); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	id, _ := s.Pending(1)
	if err := s.AcknowledgePresentation(id, stamp(1, 100)); err != nil {
		t.Fatalf("AcknowledgePresentation: %v", err)
	}
	if err := s.SubmitOrder(order("o1", market.SideBuy, 1), gesture("g-1", 1, 150)); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
	before := s.JournalLen()

	if err := s.AcknowledgePresentation(id, stamp(1, 100)); err != nil {
		t.Fatalf("a retry of a confirmed presentation was refused: %v", err)
	}
	if s.JournalLen() != before {
		t.Fatalf("journal: got %d events, want the %d it had", s.JournalLen(), before)
	}
	checked(t, s)
}

// Scenario: a resumed session continues the chronology it was interrupted in
//
//	Given a journal cut after a decision at 5000ns into segment 1
//	When it is replayed and resumed
//	Then a decision at 10ns into that same segment is still refused. The clock
//	  is journal state, not session state, and forgetting it would let a
//	  recovery launder a reading the uninterrupted run would have refused.
func TestResumeCarriesTheChronology(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	if err := s.SubmitOrder(order("o1", market.SideBuy, 1), gesture("g-1", 1, 90_000_000)); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}

	state, err := session.Replay(s.Events())
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	resumed, err := session.Resume(state, nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	err = resumed.SubmitOrder(order("o2", market.SideBuy, 1), gesture("g-2", 1, 10_000))
	if !errors.Is(err, session.ErrInteractionOrder) {
		t.Fatalf("SubmitOrder after resume: got %v, want %v", err, session.ErrInteractionOrder)
	}
}

// Scenario: both readers hold a presentation to the same chronology
//
//	Given a journal in which a presentation's stamp is incoherent
//	Then Replay and Verify refuse it, whichever way it is incoherent. Until the
//	  machine knew about presentations, all three of these were accepted.
func TestBothReadersHoldAPresentationToTheChronology(t *testing.T) {
	build := func(t *testing.T, mutate func([]session.Event)) []session.Event {
		t.Helper()
		s := newSession(t)
		mustOpen(t, s, 2_000, "d1")
		if err := s.Observe(sized(3_000, 20_000, 20_001, 50), 1); err != nil {
			t.Fatalf("Observe: %v", err)
		}
		id, _ := s.Pending(1)
		if err := s.AcknowledgePresentation(id, stamp(1, 100)); err != nil {
			t.Fatalf("AcknowledgePresentation: %v", err)
		}
		if err := s.SubmitOrder(order("o1", market.SideBuy, 1), gesture("g-1", 1, 150)); err != nil {
			t.Fatalf("SubmitOrder: %v", err)
		}
		events := s.Events()
		mutate(events)
		return events
	}

	presentationAt := func(events []session.Event, at session.Instant) {
		for n, e := range events {
			if v, ok := e.(session.ObservationPresented); ok {
				v.Presented = at
				events[n] = v
			}
		}
	}

	for _, tc := range []struct {
		name   string
		mutate func([]session.Event)
	}{
		{"later in its segment than the decision it answers", func(e []session.Event) {
			presentationAt(e, stamp(1, 9_000))
		}},
		{"in a segment the log had already left", func(e []session.Event) {
			presentationAt(e, stamp(1, 100))
			for n, ev := range e {
				if v, ok := ev.(session.OrderSubmitted); ok {
					v.Decided = gesture(v.Decided.GestureID, 5, 150)
					e[n] = v
				}
			}
			// The presentation now sits in segment 1, the decision in 5, and a
			// second presentation could not go back — so move the first one
			// forward instead and put the decision behind it.
			presentationAt(e, stamp(6, 100))
		}},
		{"carrying no stamp at all", func(e []session.Event) {
			presentationAt(e, session.Instant{})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := build(t, tc.mutate)
			if _, err := session.Replay(events); err == nil {
				t.Fatal("Replay accepted it")
			}
			if err := session.Verify(events); err == nil {
				t.Fatal("Verify accepted it")
			}
		})
	}
}

// observedSequence is the journal position of the observation on the screen,
// read back from the log so a test can name a presentation the session would
// not offer it.
func observedSequence(t *testing.T, s *session.Session) uint64 {
	t.Helper()
	for _, e := range s.Events() {
		if e.Header().Kind == session.KindMarketObserved {
			return e.Header().Sequence
		}
	}
	t.Fatal("no observation in the journal")
	return 0
}

// Scenario: one definition of a half-written stamp, asked by both callers
//
//	Given every combination of a moment present or absent with an act named or
//	  not
//	Then Decision.Malformed and the command door answer identically, because
//	  they are the same function — and this is what holds them to that as the
//	  two grow apart.
func TestOneDefinitionOfAHalfWrittenStamp(t *testing.T) {
	for _, tc := range []struct {
		name      string
		decision  session.Decision
		malformed bool
	}{
		{"wholly absent", session.Decision{}, false},
		{"wholly present", gesture("g-1", 1, 90_000_000), false},
		{"an act with no moment", session.Decision{GestureID: "g-1"}, true},
		{"a moment with no act", session.Decision{
			AtUTCNanos: 1, Segment: 1, ElapsedNanos: 90_000_000,
		}, true},
		{"a segment with no world clock", session.Decision{
			GestureID: "g-1", Segment: 1, ElapsedNanos: 90_000_000,
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.decision.Malformed(); got != tc.malformed {
				t.Fatalf("Decision.Malformed: got %v, want %v", got, tc.malformed)
			}

			s := newSession(t)
			mustOpen(t, s, 2_000, "d1")
			mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
			err := s.SubmitOrder(order("o1", market.SideBuy, 1), tc.decision)
			gate := errors.Is(err, session.ErrMalformedDecision)
			if gate != tc.malformed {
				t.Fatalf("the door said malformed=%v, Decision.Malformed says %v (err %v)",
					gate, tc.malformed, err)
			}
		})
	}
}

// Scenario: Verify alone still refuses a run that is two claims at once
//
//	Given a journal whose configuration says nobody traded it and names who did
//	Then Verify refuses it without Replay's help.
//
// The clock reads pacing rather than the subject now, so a journal with no
// stamped event in it would have slipped past a Verify that never asked whether
// the two agreed. Nothing calls Verify without Replay, so this was never a
// hole — it was Verify quietly becoming less strong on its own than it was.
func TestVerifyAloneRefusesAContradictoryRun(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")

	events := s.Events()
	started := events[0].(session.SessionStarted)
	started.Config.Pacing = session.PacingScripted // and t-01 is still named
	events[0] = started

	if err := session.Verify(events); !errors.Is(err, session.ErrContradictoryLog) {
		t.Fatalf("Verify: got %v, want %v", err, session.ErrContradictoryLog)
	}
	if _, err := session.Replay(events); !errors.Is(err, session.ErrStructure) {
		t.Fatalf("Replay: got %v, want %v", err, session.ErrStructure)
	}
}

// Scenario: what the log required names no act either
//
//	Given an event the events before it demanded — a leg its sibling cancelled,
//	  an ending a fill required
//	When it carries a gesture identifier and no moment
//	Then both readers refuse it.
//
// The moment and the act are two halves of one rule and a derived event is held
// to both: its act must be absent exactly as its moment is. Holding only the
// moment let a journal name a person's act on something nobody ordered — inert,
// because nothing reads a gesture off a derived event, but half a claim that
// somebody was behind what the log demanded of itself, in a record whose whole
// purpose is that only real acts are named.
func TestWhatTheLogRequiredNamesNoAct(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind session.Kind
	}{
		{"a leg its sibling cancelled", session.KindOrderCancelled},
		{"an ending a fill required", session.KindProtectionEnded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := protectedLong(t)
			mustObserve(t, s, sized(4_000, 20_500, 20_501, 50))
			events := s.Events()
			at := indexOfKind(t, events, tc.kind, 1)

			switch v := events[at].(type) {
			case session.OrderCancelled:
				v.Decided = session.Decision{GestureID: "g-forged"}
				events[at] = v
			case session.ProtectionEnded:
				v.Decided = session.Decision{GestureID: "g-forged"}
				events[at] = v
			}

			// The readers wrap the inner cause with %v, so the sentinel to
			// match is theirs and the sentence is what says which rule fired.
			_, err := session.Replay(events)
			if !errors.Is(err, session.ErrStructure) {
				t.Fatalf("Replay: got %v, want %v", err, session.ErrStructure)
			}
			if !strings.Contains(err.Error(), `gesture "g-forged"`) {
				t.Fatalf("Replay rejected it for another reason: %v", err)
			}
			err = session.Verify(events)
			if !errors.Is(err, session.ErrContradictoryLog) {
				t.Fatalf("Verify: got %v, want %v", err, session.ErrContradictoryLog)
			}
			if !strings.Contains(err.Error(), `gesture "g-forged"`) {
				t.Fatalf("Verify rejected it for another reason: %v", err)
			}
		})
	}
}

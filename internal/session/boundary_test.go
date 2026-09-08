package session_test

import (
	"errors"
	"testing"

	"praxis/internal/session"
)

// Scenario: a trading session that ended is not still open under its own name
//
//	Given a session whose trading session has ended
//	Then nothing reports one open, and nothing reports the identifier of the
//	  one that closed as the identifier of an open one.
//
// Openness was inferred from a non-empty identifier by three separate readers,
// and the identifier outlived the session it named. A journal whose last
// confirmed batch is a SessionEnded — what a crash between two boundary
// commands leaves, and what an applied repair produces — replayed to a state
// reporting no session open and the closed one's name, and every consumer that
// asked the name instead of the state carried on into the next boundary as
// though one were still running.
func TestATradingSessionThatEndedIsNotOpen(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	if !s.TradingSessionOpen() {
		t.Fatal("an open trading session reports itself closed")
	}
	if s.OpenSessionID() != "d1" {
		t.Fatalf("open session: got %q, want d1", s.OpenSessionID())
	}

	if err := s.EndTradingSession(4_000); err != nil {
		t.Fatalf("EndTradingSession: %v", err)
	}
	if s.TradingSessionOpen() {
		t.Fatal("a trading session that ended reports itself open")
	}
	if s.OpenSessionID() != "" {
		t.Fatalf("open session after it ended: got %q, want none", s.OpenSessionID())
	}

	// And a reader rebuilding the same journal says the same thing.
	state, err := session.Replay(s.Events())
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if state.SessionOpen {
		t.Fatal("Replay reports a session open after it ended")
	}
	if state.CurrentSessionID != "" {
		t.Fatalf("Replay carries %q forward as the open session", state.CurrentSessionID)
	}
	resumed, err := session.Resume(state, nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.TradingSessionOpen() || resumed.OpenSessionID() != "" {
		t.Fatalf("a resumed session reports %q open", resumed.OpenSessionID())
	}
}

// Scenario: a resumed session is only as coherent as the state it came from
//
//	Given a replayed state whose two claims about the trading session disagree
//	Then Resume refuses it, in both directions.
//
// Replay never builds one: the pair moves together, in one place each. But
// ReplayedState is an exported struct of exported fields, so one can be
// composed — and a session resumed from it is past every door the constructor
// holds. Open with nothing to name, EndTradingSession succeeds and records a
// boundary whose identifier is empty: a valid command the record cannot write,
// which kills the session at commit. That is the identifier rule's own failure,
// reached through a type instead of through a name.
func TestResumeRefusesAStateAtOddsWithItself(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))

	for _, tc := range []struct {
		name string
		bend func(*session.ReplayedState)
	}{
		{"open under no name", func(st *session.ReplayedState) { st.CurrentSessionID = "" }},
		{"named with none open", func(st *session.ReplayedState) { st.SessionOpen = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, err := session.Replay(s.Events())
			if err != nil {
				t.Fatalf("Replay: %v", err)
			}
			tc.bend(state)
			if _, err := session.Resume(state, nil); !errors.Is(err, session.ErrIncoherentState) {
				t.Fatalf("Resume: got %v, want %v", err, session.ErrIncoherentState)
			}
		})
	}

	// And the state Replay actually produces still resumes.
	state, err := session.Replay(s.Events())
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if _, err := session.Resume(state, nil); err != nil {
		t.Fatalf("a coherent state was refused: %v", err)
	}
}

package marketdata_test

import (
	"testing"

	"praxis/internal/adapters/marketdata"
	"praxis/internal/session"
)

// Scenario: a run resumed on a boundary carries on across it
//
//	Given a journal whose last confirmed batch closed a trading session
//	When it is replayed, resumed, and driven into the rest of the file
//	Then the next boundary opens, and the resumed run produces the journal an
//	  uninterrupted one would have.
//
// This is the crash between two boundary commands: a trading session ended and
// the next had not opened. Replay reported no session open and carried the
// closed one's name forward; Drive read the name rather than the state, saw the
// file move to another session, and ended one that was already over — so the
// journal was permanently unrunnable while every diagnostic called it clean.
func TestAResumeOnABoundaryCrossesIt(t *testing.T) {
	const twoSessions = "3000,1,d1,20000,20001,10,10\n" +
		"4000,1,d1,20010,20011,10,10\n" +
		"5000,1,d2,19990,19991,10,10\n" +
		"6000,1,d2,19985,19986,10,10\n"
	feed := feedFile(t, twoSessions)

	// The uninterrupted run, for comparison.
	whole := drivenEvents(t, feed)

	// A run that stopped exactly on the boundary: the first session's rows,
	// and then the boundary itself, with nothing after it.
	s, err := session.New(config(), 1_000, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := marketdata.Drive(s, feedFile(t, "3000,1,d1,20000,20001,10,10\n4000,1,d1,20010,20011,10,10\n"), 0); err != nil {
		t.Fatalf("Drive: %v", err)
	}
	if err := s.EndTradingSession(5_000); err != nil {
		t.Fatalf("EndTradingSession: %v", err)
	}
	interrupted := s.Events()

	state, err := session.Replay(interrupted)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if state.SessionOpen || state.CurrentSessionID != "" {
		t.Fatalf("Replay carries %q forward as open=%v", state.CurrentSessionID, state.SessionOpen)
	}
	resumed, err := session.Resume(state, nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	consumed, err := marketdata.Consumed(interrupted, feed)
	if err != nil {
		t.Fatalf("Consumed: %v", err)
	}
	if err := marketdata.Drive(resumed, feed, consumed); err != nil {
		t.Fatalf("Drive after resuming on a boundary: %v", err)
	}

	got, want := resumed.Events(), whole
	if len(got) != len(want) {
		t.Fatalf("events: got %d, want the %d an uninterrupted run produced", len(got), len(want))
	}
	for n := range want {
		if got[n] != want[n] {
			t.Fatalf("event %d: got %+v, want %+v", n, got[n], want[n])
		}
	}
}

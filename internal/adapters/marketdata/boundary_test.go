package marketdata_test

import (
	"testing"

	"praxis/internal/adapters/marketdata"
	"praxis/internal/market"
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

// Scenario: a run stops where the evaluation stops, and stops cleanly
//
//	Given a file whose second trading session lies past the point the
//	  evaluation ended
//	Then Drive returns without error, the journal holds no boundary written
//	  after the evaluation ended, and the rows past that point are simply not
//	  consumed.
//
// The check is at the top of the loop rather than in reaction to the kernel's
// refusal, because the boundary logic runs before the observation does.
// Reacting would close the trading session first and commit a SessionEnded that
// the policy says should not exist — the run would still stop, and the journal
// would carry an act nobody performed.
func TestARunStopsWhereTheEvaluationStops(t *testing.T) {
	const crashThenAnotherSession = "3000,1,d1,20000,20001,50,50\n" +
		"4000,1,d1,19000,19001,50,50\n" +
		"5000,1,d2,19990,19991,50,50\n" +
		"6000,1,d2,19985,19986,50,50\n"
	feed := feedFile(t, crashThenAnotherSession)

	cfg := config()
	cfg.Rules.MaxDailyLossCts = 100_000
	s, err := session.New(cfg, 1_000, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := marketdata.Drive(s, &marketdata.Feed{
		Instrument: feed.Instrument, Observations: feed.Observations[:1],
	}, 0); err != nil {
		t.Fatalf("Drive: %v", err)
	}
	buy, err := market.NewMarketOrder("o-1", feed.Instrument, market.SideBuy, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitOrder(buy, session.Decision{}); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}

	if err := marketdata.Drive(s, feed, 1); err != nil {
		t.Fatalf("Drive past a terminal evaluation: %v", err)
	}
	if !s.ChallengeEnded() {
		t.Fatal("the evaluation did not end, so this proves nothing")
	}

	events := s.Events()
	if last := events[len(events)-1]; last.Header().Kind == session.KindSessionEnded {
		t.Fatal("a boundary was written after the evaluation ended")
	}
	if !s.TradingSessionOpen() {
		t.Fatal("the trading session was closed on the evaluation's behalf")
	}

	// The rows past the end are not in the journal, and the reader says so
	// rather than calling the journal a mismatch for the file.
	consumed, err := marketdata.Consumed(events, feed)
	if err != nil {
		t.Fatalf("Consumed: %v", err)
	}
	if consumed != 2 {
		t.Fatalf("consumed: got %d rows, want the 2 taken before the end", consumed)
	}

	// And running it again changes nothing.
	before := len(events)
	if err := marketdata.Drive(s, feed, consumed); err != nil {
		t.Fatalf("Drive again: %v", err)
	}
	if len(s.Events()) != before {
		t.Fatalf("a second run added %d events", len(s.Events())-before)
	}
}

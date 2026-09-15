package marketdata_test

import (
	"errors"
	"reflect"
	"testing"

	"praxis/internal/adapters/marketdata"
	"praxis/internal/market"
	"praxis/internal/session"
)

// Step is one row of Drive, and there is exactly one implementation of a row:
// Drive is a loop over Step. These tests hold that the interface, which advances
// one row at a time, and a driven run cannot come apart — the failure the two
// divergent instrument checks already showed is what two copies of a rule do.

const twoSessionsFourRows = "3000,1,d1,20000,20001,10,10\n" +
	"4000,1,d1,20002,20003,10,10\n" +
	"5000,1,d2,19990,19991,10,10\n" +
	"6000,1,d2,19985,19986,10,10\n"

// Scenario: stepping row by row writes the journal driving does
func TestSteppingEveryRowIsDriving(t *testing.T) {
	feed := feedFile(t, twoSessionsFourRows)

	driven, err := session.New(config(), 1_000, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := marketdata.Drive(driven, feed, 0); err != nil {
		t.Fatalf("Drive: %v", err)
	}

	stepped, err := session.New(config(), 1_000, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for at := 0; at < len(feed.Observations); {
		next, err := marketdata.Step(stepped, feed, at)
		if err != nil {
			t.Fatalf("Step(%d): %v", at, err)
		}
		if next != at+1 {
			t.Fatalf("Step(%d) returned %d, want %d", at, next, at+1)
		}
		at = next
	}

	if !reflect.DeepEqual(stepped.Events(), driven.Events()) {
		t.Fatalf("stepping and driving wrote different journals:\n stepped %d events\n driven  %d events",
			len(stepped.Events()), len(driven.Events()))
	}
}

// Scenario: one step crosses a trading-session boundary on its own
//
// The row whose identifier changes ends the open session and opens the next one
// before it is observed. A caller stepping by hand must not have to know that.
func TestAStepCrossesABoundaryOnItsOwn(t *testing.T) {
	feed := feedFile(t, twoSessionsFourRows)
	s, err := session.New(config(), 1_000, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for at := 0; at < 2; at++ {
		if _, err := marketdata.Step(s, feed, at); err != nil {
			t.Fatalf("Step(%d): %v", at, err)
		}
	}
	if s.OpenSessionID() != "d1" {
		t.Fatalf("before the boundary the open session is %q, want d1", s.OpenSessionID())
	}
	if _, err := marketdata.Step(s, feed, 2); err != nil {
		t.Fatalf("Step across the boundary: %v", err)
	}
	if s.OpenSessionID() != "d2" {
		t.Fatalf("after the boundary the open session is %q, want d2", s.OpenSessionID())
	}
}

// Scenario: a step past the last row is refused and changes nothing
//
// Reaching the end of a file is not a boundary (ADR-011), and it is not a step
// either. A caller asked for an observation and none exists, so it is told so
// rather than answered as though one had been shown.
func TestAStepPastTheLastRowIsRefused(t *testing.T) {
	feed := feedFile(t, twoSessionsFourRows)
	s, err := session.New(config(), 1_000, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := marketdata.Drive(s, feed, 0); err != nil {
		t.Fatalf("Drive: %v", err)
	}
	before := s.JournalLen()

	next, err := marketdata.Step(s, feed, len(feed.Observations))
	if !errors.Is(err, marketdata.ErrFeedExhausted) {
		t.Fatalf("Step past the end: got %v, want ErrFeedExhausted", err)
	}
	if next != len(feed.Observations) || s.JournalLen() != before {
		t.Fatalf("a refused step moved something: next %d, journal %d -> %d", next, before, s.JournalLen())
	}
}

// Scenario: a step after the evaluation ended is refused before any boundary
//
// The same rule Drive keeps, and for the same reason: reacting to the kernel's
// refusal would close the trading session first and commit a SessionEnded the
// policy says should not exist.
func TestAStepAfterTheEvaluationEndedWritesNothing(t *testing.T) {
	const crashThenAnotherSession = "3000,1,d1,20000,20001,50,50\n" +
		"4000,1,d1,19000,19001,50,50\n" +
		"5000,1,d2,19990,19991,50,50\n"
	feed := feedFile(t, crashThenAnotherSession)

	s, err := session.New(config(), 1_000, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := marketdata.Step(s, feed, 0); err != nil {
		t.Fatalf("Step: %v", err)
	}
	buy, err := market.NewMarketOrder("o-1", feed.Instrument, market.SideBuy, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitOrder(buy, session.Decision{}); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
	if _, err := marketdata.Step(s, feed, 1); err != nil {
		t.Fatalf("Step into the loss: %v", err)
	}
	if !s.ChallengeEnded() {
		t.Fatal("the evaluation did not end, so this proves nothing")
	}
	before := s.JournalLen()

	next, err := marketdata.Step(s, feed, 2)
	if !errors.Is(err, session.ErrChallengeEnded) {
		t.Fatalf("Step after the end: got %v, want ErrChallengeEnded", err)
	}
	if next != 2 || s.JournalLen() != before || !s.TradingSessionOpen() {
		t.Fatalf("a refused step moved something: next %d, journal %d -> %d, session open %v",
			next, before, s.JournalLen(), s.TradingSessionOpen())
	}
}

// Scenario: a row index outside the file is refused
func TestAStepFromOutsideTheFileIsRefused(t *testing.T) {
	feed := feedFile(t, twoSessionsFourRows)
	s, err := session.New(config(), 1_000, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, from := range []int{-1, len(feed.Observations) + 1} {
		if _, err := marketdata.Step(s, feed, from); err == nil {
			t.Fatalf("Step(%d) was accepted", from)
		}
	}
	if s.JournalLen() != 1 {
		t.Fatalf("a refused step wrote events: journal holds %d", s.JournalLen())
	}
}

// Scenario: an evaluation never ends in the batch that opens a session
//
// OpenTradingSession revalues, and a valuation can end an evaluation. If the one
// an open records ever did, the journal would hold a SessionOpened with no
// observation after it — a session opened on an evaluation it then ended, which
// is what "the journal ends where the evaluation ends" says a run must not do.
//
// Through Step it cannot, and this holds the reason rather than the conclusion.
// Step never lets an observation land with no session open: it ends a session and
// opens the next in the same call, immediately before observing. So the quote and
// the account an open values are the ones the last valuation of the previous
// session already evaluated, and a valuation already evaluated cannot cross a
// threshold it did not cross. The daily reference is set to that same equity, so
// the daily rule cannot fire at the open either.
//
// The case is built to be the nearest one: a position carried across a boundary
// into an adverse gap large enough to end the evaluation. It ends — on the
// observation, after the open.
func TestAnEvaluationNeverEndsInTheBatchThatOpensASession(t *testing.T) {
	const gapAcrossTheBoundary = "3000,1,d1,20000,20001,50,50\n" +
		"4000,1,d1,19990,19991,50,50\n" +
		"5000,1,d2,18600,18601,50,50\n" +
		"6000,1,d2,18590,18591,50,50\n"
	feed := feedFile(t, gapAcrossTheBoundary)

	s, err := session.New(config(), 1_000, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := marketdata.Step(s, feed, 0); err != nil {
		t.Fatalf("Step: %v", err)
	}
	buy, err := market.NewMarketOrder("o-1", feed.Instrument, market.SideBuy, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitOrder(buy, session.Decision{}); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
	if err := marketdata.Drive(s, feed, 1); err != nil {
		t.Fatalf("Drive: %v", err)
	}
	if !s.ChallengeEnded() {
		t.Fatal("the evaluation did not end, so this proves nothing")
	}

	events := s.Events()
	var (
		lastValued   *session.AccountValued
		opened       bool
		openSequence uint64
		compared     int
	)
	for _, e := range events {
		switch v := e.(type) {
		case session.SessionOpened:
			opened, openSequence = true, v.Sequence
		case session.AccountValued:
			// The first valuation after an open is the one the open recorded,
			// and it must repeat the last valuation of the session before it.
			if opened && lastValued != nil {
				compared++
				if v.BalanceCts != lastValued.BalanceCts || v.EquityCts != lastValued.EquityCts {
					t.Fatalf("the open at sequence %d valued balance %d equity %d; the last valuation before it was %d, %d",
						openSequence, v.BalanceCts, v.EquityCts, lastValued.BalanceCts, lastValued.EquityCts)
				}
			}
			opened = false
			valued := v
			lastValued = &valued
		}
	}

	// The comparison is the claim, so it has to have been made: an open after a
	// session with a valuation in it.
	if compared == 0 {
		t.Fatal("no open followed a valued session, so no open valuation was compared")
	}

	var lastOpened, lastObserved uint64
	for _, e := range events {
		switch e.(type) {
		case session.SessionOpened:
			lastOpened = e.Header().Sequence
		case session.MarketObserved:
			lastObserved = e.Header().Sequence
		}
	}
	if lastObserved < lastOpened {
		t.Fatalf("the journal holds a session opened at %d with no observation after it (last at %d)",
			lastOpened, lastObserved)
	}
}

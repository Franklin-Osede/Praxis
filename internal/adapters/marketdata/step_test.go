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

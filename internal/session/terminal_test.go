package session_test

import (
	"errors"
	"testing"

	"praxis/internal/challenge"
	"praxis/internal/market"
	"praxis/internal/session"
)

// failedSession drives a session until its evaluation ends, and returns it with
// the journal it had at that moment.
func failedSession(t *testing.T) *session.Session {
	t.Helper()
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	mustSubmit(t, s, order("o1", market.SideBuy, 3))
	mustObserve(t, s, sized(4_000, 19_000, 19_001, 50))
	if s.Challenge().State() != challenge.StateFailed {
		t.Fatalf("challenge: got %v, want failed", s.Challenge().State())
	}
	return s
}

// Scenario: the journal ends where the evaluation ends
//
//	Given an evaluation that has reached passed or failed
//	When another observation arrives
//	Then it is refused, and nothing is recorded.
//
// Market after that point is market nobody can act on. Recording it would keep
// a second copy of what the file already holds, and it is not free: an
// observation belongs to a trading session, so keeping it means crossing the
// next boundary, and crossing that boundary means opening a session on an
// evaluation that is over. The terminal state of that machine is terminal in
// both directions, and it is not worth reopening to store a quote nobody can
// trade against.
//
// The rule lives here and not only in the adapter that drives the file, because
// the second adapter — an interface feeding observations by hand — has to be
// held to it too, and a rule that lives in one caller is a rule the next caller
// does not have.
func TestAnObservationAfterTheEvaluationEndsIsRefused(t *testing.T) {
	s := failedSession(t)
	before := s.JournalLen()

	err := s.Observe(sized(5_000, 19_500, 19_501, 50), 1)
	if !errors.Is(err, session.ErrChallengeEnded) {
		t.Fatalf("Observe: got %v, want %v", err, session.ErrChallengeEnded)
	}
	if s.JournalLen() != before {
		t.Fatalf("journal: got %d events, want the %d it had", s.JournalLen(), before)
	}
	if err := s.NeedsRecovery(); err != nil {
		t.Fatalf("a refusal made the session unusable: %v", err)
	}
	checked(t, s)
}

// Scenario: closing the books after an evaluation ends is the operator's to do
//
//	Given an evaluation that has ended with a trading session still open
//	Then ending that trading session is accepted.
//
// The asymmetry with OpenTradingSession reads like an oversight and is not.
// Opening a session on an evaluation that is over would ask the challenge
// engine to leave a terminal state; closing one asks nothing of it. It was only
// ever a defect when it happened automatically inside a run that then could not
// continue, and the check at the top of Drive removes that run.
func TestEndingATradingSessionAfterTheEvaluationEndsIsAllowed(t *testing.T) {
	s := failedSession(t)
	if err := s.EndTradingSession(5_000); err != nil {
		t.Fatalf("EndTradingSession: %v", err)
	}
	if s.TradingSessionOpen() {
		t.Fatal("the trading session is still open")
	}
	if err := s.OpenTradingSession(6_000, challenge.SessionID("d2")); !errors.Is(err, session.ErrChallengeEnded) {
		t.Fatalf("OpenTradingSession: got %v, want %v", err, session.ErrChallengeEnded)
	}
	checked(t, s)
}

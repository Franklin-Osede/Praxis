package session_test

import (
	"errors"
	"reflect"
	"testing"

	"praxis/internal/challenge"
	"praxis/internal/market"
	"praxis/internal/portfolio"
	"praxis/internal/session"
)

// script is one deterministic run, expressed as commands. It is the scripted
// input the end-to-end slice is built on: no file, no feed, no clock.
func script(t *testing.T) *session.Session {
	t.Helper()
	s := newSession(t)

	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, quote(3_000, 20_000, 20_001))
	mustSubmit(t, s, order("o-1", market.SideBuy, 3))
	mustObserve(t, s, quote(4_000, 20_010, 20_011))
	mustSubmit(t, s, order("o-2", market.SideSell, 1))
	mustObserve(t, s, quote(5_000, 19_990, 19_991))
	mustSubmit(t, s, order("o-3", market.SideSell, 2))
	mustSubmit(t, s, order("o-4", market.SideBuy, 2))
	if err := s.EndTradingSession(6_000); err != nil {
		t.Fatalf("EndTradingSession: %v", err)
	}

	mustOpen(t, s, 7_000, "d2")
	mustObserve(t, s, quote(8_000, 19_995, 19_996))
	mustSubmit(t, s, order("o-5", market.SideSell, 2))
	mustObserve(t, s, quote(9_000, 19_985, 19_986))
	if err := s.EndTradingSession(10_000); err != nil {
		t.Fatalf("EndTradingSession: %v", err)
	}
	return s
}

// Scenario: a session reconstructs from its journal with nothing lost
//
//	Given a scripted session
//	When the account and the evaluation are rebuilt from the journal alone
//	Then they are identical to the ones that produced it.
//
// Reconstruction consumes only the configuration, the fills, the boundaries
// and the valuations. If it needed an observation or a decision, the log would
// be describing state it does not contain.
func TestASessionReconstructsFromItsJournal(t *testing.T) {
	s := script(t)

	account, eval, err := session.Replay(s.Journal().Events())
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}

	if !reflect.DeepEqual(account.Positions(), s.Account().Positions()) {
		t.Fatalf("positions\n got: %+v\nwant: %+v", account.Positions(), s.Account().Positions())
	}
	if account.RealisedCts() != s.Account().RealisedCts() {
		t.Fatalf("realised: got %d, want %d", account.RealisedCts(), s.Account().RealisedCts())
	}
	if account.FeesCts() != s.Account().FeesCts() {
		t.Fatalf("fees: got %d, want %d", account.FeesCts(), s.Account().FeesCts())
	}
	if got, want := describe(eval), describe(s.Challenge()); got != want {
		t.Fatalf("challenge\n got: %+v\nwant: %+v", got, want)
	}
}

type challengeState struct {
	State        challenge.State
	Reason       challenge.FailureReason
	SessionID    challenge.SessionID
	ReferenceCts market.Cents
	StartingCts  market.Cents
	FloorCts     market.Cents
	FloorOn      bool
	HighWaterCts market.Cents
	HighWaterOn  bool
	TrailingCts  market.Cents
	TrailingOn   bool
}

func describe(c *challenge.Challenge) challengeState {
	floor, floorOn := c.StaticFloor()
	high, highOn := c.HighWater()
	trailing, trailingOn := c.TrailingThresholdCts()
	return challengeState{
		State: c.State(), Reason: c.FailureReason(), SessionID: c.SessionID(),
		ReferenceCts: c.ReferenceEquityCts(), StartingCts: c.StartingBalanceCts(),
		FloorCts: floor, FloorOn: floorOn,
		HighWaterCts: high, HighWaterOn: highOn,
		TrailingCts: trailing, TrailingOn: trailingOn,
	}
}

// Scenario: the recorded context never contradicts the stream
//
// Every derived figure on a decision could also be computed from the events
// before it. Storing it is deliberate, but it means the log could hold two
// truths at once. This proves it holds one.
func TestTheRecordedContextAgreesWithTheStream(t *testing.T) {
	s := script(t)
	if err := session.Verify(s.Journal().Events()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyCatchesAContradictoryContext(t *testing.T) {
	s := script(t)
	events := s.Journal().Events()

	for n, e := range events {
		if o, ok := e.(session.OrderSubmitted); ok {
			o.Context.ConsecutiveLosses++
			events[n] = o
			break
		}
	}

	if err := session.Verify(events); !errors.Is(err, session.ErrContradictoryLog) {
		t.Fatalf("error: got %v, want %v", err, session.ErrContradictoryLog)
	}
}

// Property: the same commands produce the same journal, event for event, and
// the same final state, on every run.
func TestPropertyTheSameScriptReplaysIdentically(t *testing.T) {
	const runs = 20

	baseline := script(t).Journal().Events()
	baselineAccount, baselineEval, err := session.Replay(baseline)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}

	for run := 1; run < runs; run++ {
		events := script(t).Journal().Events()
		if !reflect.DeepEqual(events, baseline) {
			t.Fatalf("run %d produced a different journal", run)
		}

		account, eval, err := session.Replay(events)
		if err != nil {
			t.Fatalf("run %d Replay: %v", run, err)
		}
		if !reflect.DeepEqual(account.Positions(), baselineAccount.Positions()) ||
			account.RealisedCts() != baselineAccount.RealisedCts() ||
			account.FeesCts() != baselineAccount.FeesCts() {
			t.Fatalf("run %d reconstructed a different account", run)
		}
		if describe(eval) != describe(baselineEval) {
			t.Fatalf("run %d reconstructed a different evaluation", run)
		}
	}
}

// The journal is one contiguous ordering: a rejected command leaves no gap.
func TestTheJournalIsOneContiguousOrdering(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")

	// Three commands that must be rejected, none of which may be recorded.
	if err := s.OpenTradingSession(3_000, "d2"); err == nil {
		t.Fatal("opening a second session was accepted")
	}
	if err := s.SubmitOrder(order("o-1", market.SideBuy, 1)); err == nil {
		t.Fatal("an order without an observation was accepted")
	}
	other := quote(3_000, 20_000, 20_001)
	other.Instrument = market.Instrument{Symbol: "MES", CentsPerTick: 125}
	if err := s.Observe(other); err == nil {
		t.Fatal("an observation for another instrument was accepted")
	}

	mustObserve(t, s, quote(3_000, 20_000, 20_001))
	mustSubmit(t, s, order("o-1", market.SideBuy, 1))

	for n, e := range s.Journal().Events() {
		if got, want := e.Header().Sequence, uint64(n+1); got != want {
			t.Fatalf("event %d carries sequence %d: a rejected command left a gap", n, got)
		}
	}
	if _, _, err := session.Replay(s.Journal().Events()); err != nil {
		t.Fatalf("Replay: %v", err)
	}
}

// A journal that does not begin with its configuration cannot be replayed.
func TestReplayRequiresTheConfiguration(t *testing.T) {
	s := script(t)
	if _, _, err := session.Replay(s.Journal().Events()[1:]); !errors.Is(err, session.ErrNoSessionStarted) {
		t.Fatalf("error: got %v, want %v", err, session.ErrNoSessionStarted)
	}
	if _, _, err := session.Replay(nil); !errors.Is(err, session.ErrNoSessionStarted) {
		t.Fatalf("error: got %v, want %v", err, session.ErrNoSessionStarted)
	}
}

var _ = portfolio.PositionEvent{}

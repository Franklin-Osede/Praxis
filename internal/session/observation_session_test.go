package session_test

import (
	"errors"
	"testing"

	"praxis/internal/market"
	"praxis/internal/session"
)

// An observation belongs to a trading session. Waiting orders and protections
// are offered each observation before the account is revalued, so the valuation
// an observation records already contains what that observation caused. A quote
// accepted with no session open broke that: it moved the last book, nothing was
// offered it, and the next open valued the account against a price no stop had
// seen — which could end the evaluation inside the batch that opened a session.
// These tests hold the rule at the writer, the same rule at the reader, and the
// invariant the rule makes true by construction.

// carriedLong opens a scripted session, buys three contracts and marks them
// against a small adverse move, leaving a position open for a boundary to carry.
func carriedLong(t *testing.T) *session.Session {
	t.Helper()
	cfg := config()
	cfg.SubjectID, cfg.Pacing = "", session.PacingScripted
	s, err := session.New(cfg, 1_000, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.OpenTradingSession(2_000, "d1"); err != nil {
		t.Fatalf("OpenTradingSession: %v", err)
	}
	if err := s.Observe(sized(3_000, 20_000, 20_001, 50), 1); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if err := s.SubmitOrder(order("o-1", market.SideBuy, 3), session.Decision{}); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
	if err := s.Observe(sized(4_000, 19_990, 19_991, 50), 2); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	return s
}

// adverseGap is a quote far enough below the entry to end the evaluation.
func adverseGap(at market.LogicalTime) market.Quote { return sized(at, 18_600, 18_601, 50) }

// Scenario: an observation with no session open is refused and records nothing
//
// Refused before anything is written, as an order is: a quote that the kernel
// could not offer to anything waiting must not become the last book either.
func TestAnObservationWithNoSessionOpenIsRefusedAndRecordsNothing(t *testing.T) {
	t.Run("before the first session opens", func(t *testing.T) {
		cfg := config()
		cfg.SubjectID, cfg.Pacing = "", session.PacingScripted
		s, err := session.New(cfg, 1_000, nil)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		before := s.JournalLen()
		if err := s.Observe(sized(2_000, 20_000, 20_001, 50), 1); !errors.Is(err, session.ErrNoSessionOpen) {
			t.Fatalf("Observe before any session: got %v, want ErrNoSessionOpen", err)
		}
		if s.JournalLen() != before || s.NeedsRecovery() != nil {
			t.Fatalf("a refused observation changed the session: journal %d -> %d, recovery %v",
				before, s.JournalLen(), s.NeedsRecovery())
		}
		if _, has := s.LastQuote(); has {
			t.Fatal("a refused observation became the last book")
		}
	})

	t.Run("between two sessions", func(t *testing.T) {
		s := carriedLong(t)
		if err := s.EndTradingSession(5_000); err != nil {
			t.Fatalf("EndTradingSession: %v", err)
		}
		before := s.JournalLen()
		lastBook, _ := s.LastQuote()

		if err := s.Observe(adverseGap(6_000), 3); !errors.Is(err, session.ErrNoSessionOpen) {
			t.Fatalf("Observe between sessions: got %v, want ErrNoSessionOpen", err)
		}
		if s.JournalLen() != before {
			t.Fatalf("a refused observation wrote events: journal %d -> %d", before, s.JournalLen())
		}
		if book, _ := s.LastQuote(); book != lastBook {
			t.Fatalf("a refused observation moved the last book to %+v", book)
		}
	})
}

// Scenario: the reader refuses what the writer refuses
//
// One rule, writer and readers. Without it a journal rewritten to put a quote
// between two sessions — which no writer can now produce — would replay, and the
// asymmetry this repository has already been bitten by twice would be back.
func TestReplayRefusesAnObservationWithNoSessionOpen(t *testing.T) {
	t.Run("before the first session opens", func(t *testing.T) {
		cfg := config()
		cfg.SubjectID, cfg.Pacing = "", session.PacingScripted
		s, err := session.New(cfg, 1_000, nil)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		forged := append(s.Events(), session.MarketObserved{
			Envelope: session.Envelope{Time: 2_000, Sequence: 2, Kind: session.KindMarketObserved},
			Quote:    sized(2_000, 20_000, 20_001, 50), SourceSequence: 1,
		})
		if _, err := session.Replay(forged); !errors.Is(err, session.ErrStructure) {
			t.Fatalf("Replay of a quote before any session: got %v, want ErrStructure", err)
		}
	})

	t.Run("between two sessions", func(t *testing.T) {
		s := carriedLong(t)
		if err := s.EndTradingSession(5_000); err != nil {
			t.Fatalf("EndTradingSession: %v", err)
		}
		events := s.Events()
		next := events[len(events)-1].Header().Sequence + 1
		forged := append(events, session.MarketObserved{
			Envelope: session.Envelope{Time: 6_000, Sequence: next, Kind: session.KindMarketObserved},
			Quote:    adverseGap(6_000), SourceSequence: 3,
		})
		if _, err := session.Replay(forged); !errors.Is(err, session.ErrStructure) {
			t.Fatalf("Replay of a quote between sessions: got %v, want ErrStructure", err)
		}
	})
}

// Scenario: an evaluation cannot end in the batch that opens a session
//
// With observations confined to sessions this is true by construction, and the
// test holds the construction. Between the last observation of one session and
// the open of the next nothing can change the book or the account: no quote is
// accepted, and fills only happen when a quote is offered. So the valuation an
// open records is the last valuation of the session before it, which was already
// evaluated against the static floor, the trailing threshold and the target; the
// daily reference is set to that same equity, so the daily rule reads zero.
//
// The case is the one that used to end at the open: a position carried across a
// boundary into an adverse gap. The gap is refused while no session is open, the
// open repeats the last valuation and leaves the evaluation active, and the gap
// ends it when it arrives inside the session — on the observation, with the
// position's protections offered it first.
func TestAnEvaluationCannotEndInTheBatchThatOpensASession(t *testing.T) {
	s := carriedLong(t)
	before := lastValuation(t, s)

	if err := s.EndTradingSession(5_000); err != nil {
		t.Fatalf("EndTradingSession: %v", err)
	}
	if err := s.Observe(adverseGap(6_000), 3); !errors.Is(err, session.ErrNoSessionOpen) {
		t.Fatalf("the gap was accepted with no session open: %v", err)
	}
	if err := s.OpenTradingSession(7_000, "d2"); err != nil {
		t.Fatalf("OpenTradingSession: %v", err)
	}
	if s.ChallengeEnded() {
		t.Fatal("the evaluation ended in the batch that opened a session")
	}
	if at := lastValuation(t, s); at.BalanceCts != before.BalanceCts || at.EquityCts != before.EquityCts {
		t.Fatalf("the open valued balance %d equity %d; the last valuation before it was %d, %d",
			at.BalanceCts, at.EquityCts, before.BalanceCts, before.EquityCts)
	}

	if err := s.Observe(adverseGap(8_000), 3); err != nil {
		t.Fatalf("Observe the gap inside the session: %v", err)
	}
	if !s.ChallengeEnded() {
		t.Fatal("the gap did not end the evaluation, so this proves nothing")
	}
	events := s.Events()
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
		t.Fatalf("the journal holds a session opened at %d with no observation after it", lastOpened)
	}
	if _, err := session.Replay(events); err != nil {
		t.Fatalf("Replay of the honest journal: %v", err)
	}
}

// Scenario: the reader refuses a quote after the evaluation has ended
//
// The other half of the same rule. The writer refuses an observation once the
// evaluation has ended — the journal ends where the evaluation ends — and the
// reader accepted one: the session is still open, and the observation left
// nothing waiting with anything to answer for. So a journal rewritten to add
// market after the failure, walking straight through a stop that is still
// waiting, replayed clean. The forgery is built on a journal the engine produces:
// a buy stop far above the market, and a position large enough that the next
// observation ends the evaluation.
func TestReplayRefusesAnObservationAfterTheEvaluationEnded(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, sized(3_000, 20_000, 20_001, 50))
	mustSubmit(t, s, stopOrder(t, "later", market.SideBuy, 1, 21_000))
	mustSubmit(t, s, order("long", market.SideBuy, 10))
	mustObserve(t, s, sized(4_000, 19_800, 19_801, 50))
	if !s.ChallengeEnded() {
		t.Fatal("the fixture did not end the evaluation, so this proves nothing")
	}
	if len(s.WorkingOrders()) != 1 {
		t.Fatalf("the fixture has no waiting stop: %+v", s.WorkingOrders())
	}
	honest := s.Events()
	if _, err := session.Replay(honest); err != nil {
		t.Fatalf("Replay of the honest journal: %v", err)
	}

	next := honest[len(honest)-1].Header().Sequence + 1
	forged := append(honest, session.MarketObserved{
		Envelope: session.Envelope{Time: 5_000, Sequence: next, Kind: session.KindMarketObserved},
		Quote:    sized(5_000, 21_100, 21_101, 50), SourceSequence: 1,
	})
	if _, err := session.Replay(forged); !errors.Is(err, session.ErrStructure) {
		t.Fatalf("Replay of market after the evaluation ended: got %v, want ErrStructure", err)
	}
}

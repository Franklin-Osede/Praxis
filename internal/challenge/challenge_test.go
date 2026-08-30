package challenge_test

import (
	"errors"
	"reflect"
	"testing"

	"praxis/internal/challenge"
	"praxis/internal/market"
)

const dailyLossCts = 100_000 // $1,000

func newChallenge(t *testing.T) *challenge.Challenge {
	t.Helper()
	c, err := challenge.New(challenge.Rules{MaxDailyLossCts: dailyLossCts})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// opened and snap describe a flat account, where balance and equity agree.
// Tests that need them to differ use openedAt and snapAt.
func opened(seq uint64, id challenge.SessionID, valuationCts market.Cents) challenge.SessionOpened {
	return openedAt(seq, id, valuationCts, valuationCts)
}

func openedAt(seq uint64, id challenge.SessionID, balanceCts, equityCts market.Cents) challenge.SessionOpened {
	return challenge.SessionOpened{
		Time:       market.LogicalTime(seq) * 1_000_000_000,
		Sequence:   seq,
		SessionID:  id,
		BalanceCts: balanceCts,
		EquityCts:  equityCts,
	}
}

func snap(seq uint64, id challenge.SessionID, valuationCts market.Cents) challenge.AccountSnapshot {
	return snapAt(seq, id, valuationCts, valuationCts)
}

func snapAt(seq uint64, id challenge.SessionID, balanceCts, equityCts market.Cents) challenge.AccountSnapshot {
	return challenge.AccountSnapshot{
		Time:       market.LogicalTime(seq) * 1_000_000_000,
		Sequence:   seq,
		SessionID:  id,
		BalanceCts: balanceCts,
		EquityCts:  equityCts,
	}
}

func mustOpen(t *testing.T, c *challenge.Challenge, o challenge.SessionOpened) []challenge.Event {
	t.Helper()
	ev, err := c.OpenSession(o)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	return ev
}

func mustObserve(t *testing.T, c *challenge.Challenge, s challenge.AccountSnapshot) []challenge.Event {
	t.Helper()
	ev, err := c.Observe(s)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	return ev
}

// Scenario: opening the first session activates a pending evaluation
//
//	Given a pending challenge
//	When its first session opens with a reference equity
//	Then the challenge is active and both the activation and the reference
//	  it established are reported.
func TestOpeningTheFirstSessionActivatesTheChallenge(t *testing.T) {
	c := newChallenge(t)
	if c.State() != challenge.StatePending {
		t.Fatalf("state: got %v, want pending", c.State())
	}

	events := mustOpen(t, c, opened(1, "2026-08-27", 5_000_000))

	if c.State() != challenge.StateActive {
		t.Fatalf("state: got %v, want active", c.State())
	}
	if c.ReferenceEquityCts() != 5_000_000 {
		t.Fatalf("reference: got %d, want 5000000", c.ReferenceEquityCts())
	}
	want := []challenge.Event{
		{Kind: challenge.ChallengeActivated, Time: 1_000_000_000, Sequence: 1, SessionID: "2026-08-27", EquityCts: 5_000_000},
		{Kind: challenge.SessionReferenceEstablished, Time: 1_000_000_000, Sequence: 1, SessionID: "2026-08-27", EquityCts: 5_000_000},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events\n got: %+v\nwant: %+v", events, want)
	}
}

// Scenario: the daily loss limit fails an evaluation
//
//	Given an active challenge with a $1,000 daily loss limit and a session
//	  reference of $50,000
//	When equity falls to exactly $49,000
//	Then the challenge is still active
//	And when it falls one cent further, the challenge is failed and the
//	  breach is reported.
func TestDailyLossLimit(t *testing.T) {
	tests := []struct {
		name      string
		equityCts market.Cents
		wantState challenge.State
		wantLoss  market.Cents
	}{
		{"a profit is not a loss", 5_100_000, challenge.StateActive, 0},
		{"unchanged equity", 5_000_000, challenge.StateActive, 0},
		{"a loss within the limit", 4_950_000, challenge.StateActive, 0},
		{"exactly at the limit", 4_900_000, challenge.StateActive, 0},
		{"one cent past the limit", 4_899_999, challenge.StateFailed, 100_001},
		{"far past the limit", 4_000_000, challenge.StateFailed, 1_000_000},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newChallenge(t)
			mustOpen(t, c, opened(1, "s1", 5_000_000))
			events := mustObserve(t, c, snap(2, "s1", tc.equityCts))

			if c.State() != tc.wantState {
				t.Fatalf("state: got %v, want %v", c.State(), tc.wantState)
			}
			if tc.wantState != challenge.StateFailed {
				if len(events) != 0 {
					t.Fatalf("events: got %+v, want none", events)
				}
				return
			}
			want := []challenge.Event{{
				Kind: challenge.ChallengeFailed, Time: 2_000_000_000, Sequence: 2,
				SessionID: "s1", EquityCts: tc.equityCts, LossCts: tc.wantLoss,
				Reason: challenge.FailureDailyLoss,
			}}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("events\n got: %+v\nwant: %+v", events, want)
			}
			if c.FailureReason() != challenge.FailureDailyLoss {
				t.Fatalf("reason: got %v, want daily loss", c.FailureReason())
			}
		})
	}
}

// Scenario: a new session establishes a new reference
//
//	Given a session that ended down $900, within the limit
//	When a new session opens at that lower equity
//	Then the reference re-bases, and a further $900 loss does not fail even
//	  though the total loss is now $1,800.
func TestANewSessionRebasesTheReference(t *testing.T) {
	c := newChallenge(t)
	mustOpen(t, c, opened(1, "day-1", 5_000_000))
	mustObserve(t, c, snap(2, "day-1", 4_910_000))

	mustOpen(t, c, opened(3, "day-2", 4_910_000))
	if c.ReferenceEquityCts() != 4_910_000 {
		t.Fatalf("reference: got %d, want 4910000", c.ReferenceEquityCts())
	}

	mustObserve(t, c, snap(4, "day-2", 4_820_000))
	if c.State() != challenge.StateActive {
		t.Fatalf("state: got %v, want active after two losses inside their own limits", c.State())
	}

	mustObserve(t, c, snap(5, "day-2", 4_809_999))
	if c.State() != challenge.StateFailed {
		t.Fatalf("state: got %v, want failed", c.State())
	}
}

// Scenario: time alone resets nothing
//
//	Given a session whose observations span more than a calendar day
//	When equity keeps falling without a new session opening
//	Then the reference does not move and the cumulative loss fails the
//	  challenge. Midnight is not a boundary; a SessionID is.
func TestTimePassingDoesNotResetTheReference(t *testing.T) {
	const day = market.LogicalTime(24 * 60 * 60 * 1_000_000_000)

	c := newChallenge(t)
	mustOpen(t, c, challenge.SessionOpened{Time: 0, Sequence: 1, SessionID: "long-session", BalanceCts: 5_000_000, EquityCts: 5_000_000})
	mustObserve(t, c, challenge.AccountSnapshot{Time: day, Sequence: 2, SessionID: "long-session", BalanceCts: 4_950_000, EquityCts: 4_950_000})

	if c.ReferenceEquityCts() != 5_000_000 {
		t.Fatalf("reference moved to %d after a day passed", c.ReferenceEquityCts())
	}
	if c.State() != challenge.StateActive {
		t.Fatalf("state: got %v, want active", c.State())
	}

	mustObserve(t, c, challenge.AccountSnapshot{Time: 3 * day, Sequence: 3, SessionID: "long-session", BalanceCts: 4_899_999, EquityCts: 4_899_999})
	if c.State() != challenge.StateFailed {
		t.Fatalf("state: got %v, want failed on the cumulative loss", c.State())
	}
}

// Scenario: a failed evaluation is terminal
func TestAFailedChallengeIsTerminal(t *testing.T) {
	c := newChallenge(t)
	mustOpen(t, c, opened(1, "s1", 5_000_000))
	mustObserve(t, c, snap(2, "s1", 4_000_000))
	if c.State() != challenge.StateFailed {
		t.Fatalf("state: got %v, want failed", c.State())
	}

	if _, err := c.Observe(snap(3, "s1", 5_500_000)); !errors.Is(err, challenge.ErrTerminal) {
		t.Fatalf("recovering observation: got %v, want %v", err, challenge.ErrTerminal)
	}
	if _, err := c.OpenSession(opened(4, "s2", 5_500_000)); !errors.Is(err, challenge.ErrTerminal) {
		t.Fatalf("new session: got %v, want %v", err, challenge.ErrTerminal)
	}
	if c.State() != challenge.StateFailed || c.FailureReason() != challenge.FailureDailyLoss {
		t.Fatalf("a failed challenge changed: %v %v", c.State(), c.FailureReason())
	}
}

// Scenario: input that cannot describe a real evaluation is rejected, and
// changes nothing
func TestRejectsImpossibleInput(t *testing.T) {
	t.Run("rules without a limit", func(t *testing.T) {
		if _, err := challenge.New(challenge.Rules{}); !errors.Is(err, challenge.ErrNonPositiveDailyLoss) {
			t.Fatalf("error: got %v, want %v", err, challenge.ErrNonPositiveDailyLoss)
		}
	})

	t.Run("observing before any session is open", func(t *testing.T) {
		c := newChallenge(t)
		if _, err := c.Observe(snap(1, "s1", 5_000_000)); !errors.Is(err, challenge.ErrNotActive) {
			t.Fatalf("error: got %v, want %v", err, challenge.ErrNotActive)
		}
		if c.State() != challenge.StatePending {
			t.Fatalf("state: got %v, want pending", c.State())
		}
	})

	t.Run("a session with no identifier", func(t *testing.T) {
		c := newChallenge(t)
		if _, err := c.OpenSession(opened(1, "", 5_000_000)); !errors.Is(err, challenge.ErrEmptySessionID) {
			t.Fatalf("error: got %v, want %v", err, challenge.ErrEmptySessionID)
		}
	})

	t.Run("a snapshot for a session that was never opened", func(t *testing.T) {
		c := newChallenge(t)
		mustOpen(t, c, opened(1, "s1", 5_000_000))
		if _, err := c.Observe(snap(2, "s2", 4_000_000)); !errors.Is(err, challenge.ErrUnknownSession) {
			t.Fatalf("error: got %v, want %v", err, challenge.ErrUnknownSession)
		}
		if c.State() != challenge.StateActive {
			t.Fatalf("a rejected snapshot failed the challenge")
		}
	})

	t.Run("a session identifier that returns", func(t *testing.T) {
		c := newChallenge(t)
		mustOpen(t, c, opened(1, "day-1", 5_000_000))
		mustOpen(t, c, opened(2, "day-2", 5_000_000))

		if _, err := c.OpenSession(opened(3, "day-1", 5_000_000)); !errors.Is(err, challenge.ErrSessionReturned) {
			t.Fatalf("error: got %v, want %v", err, challenge.ErrSessionReturned)
		}
		if c.SessionID() != "day-2" {
			t.Fatalf("session: got %v, want day-2 still open", c.SessionID())
		}
	})
}

// Scenario: inputs arrive in one strictly increasing order
func TestRejectsOutOfOrderInput(t *testing.T) {
	tests := []struct {
		name string
		next func(*challenge.Challenge) error
	}{
		{"a snapshot before the last accepted one", func(c *challenge.Challenge) error {
			_, err := c.Observe(snap(1, "s1", 4_000_000))
			return err
		}},
		{"a snapshot repeating the last position", func(c *challenge.Challenge) error {
			_, err := c.Observe(snap(2, "s1", 4_000_000))
			return err
		}},
		{"a session opening before the last accepted input", func(c *challenge.Challenge) error {
			_, err := c.OpenSession(opened(1, "s2", 5_000_000))
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newChallenge(t)
			mustOpen(t, c, opened(1, "s1", 5_000_000))
			mustObserve(t, c, snap(2, "s1", 4_990_000))

			if err := tc.next(c); !errors.Is(err, challenge.ErrOutOfOrder) {
				t.Fatalf("error: got %v, want %v", err, challenge.ErrOutOfOrder)
			}
			if c.State() != challenge.StateActive || c.SessionID() != "s1" || c.ReferenceEquityCts() != 5_000_000 {
				t.Fatalf("a rejected input mutated the challenge: %v %v %d", c.State(), c.SessionID(), c.ReferenceEquityCts())
			}
		})
	}
}

// A tie in logical time is resolved by sequence, and is not out of order.
func TestSequenceBreaksTiesInLogicalTime(t *testing.T) {
	c := newChallenge(t)
	mustOpen(t, c, challenge.SessionOpened{Time: 100, Sequence: 1, SessionID: "s1", BalanceCts: 5_000_000, EquityCts: 5_000_000})
	mustObserve(t, c, challenge.AccountSnapshot{Time: 100, Sequence: 2, SessionID: "s1", BalanceCts: 4_990_000, EquityCts: 4_990_000})

	if c.State() != challenge.StateActive {
		t.Fatalf("state: got %v, want active", c.State())
	}
}

// Property: the same inputs produce the same state and the same events, every
// time.
func TestPropertyReplayIsDeterministic(t *testing.T) {
	const runs = 20

	type outcome struct {
		state     challenge.State
		reason    challenge.FailureReason
		session   challenge.SessionID
		reference market.Cents
		events    []challenge.Event
	}

	var baseline outcome
	for run := 0; run < runs; run++ {
		c := newChallenge(t)
		var events []challenge.Event

		equity := market.Cents(5_000_000)
		seq := uint64(0)
		for day := 0; day < 5; day++ {
			seq++
			id := challenge.SessionID([]string{"d1", "d2", "d3", "d4", "d5"}[day])
			ev, err := c.OpenSession(opened(seq, id, equity))
			if err != nil {
				break
			}
			events = append(events, ev...)

			for step := 0; step < 6; step++ {
				seq++
				equity -= 20_000
				ev, err := c.Observe(snap(seq, id, equity))
				if err != nil {
					break
				}
				events = append(events, ev...)
			}
		}

		got := outcome{c.State(), c.FailureReason(), c.SessionID(), c.ReferenceEquityCts(), events}
		if run == 0 {
			baseline = got
			if got.state != challenge.StateFailed {
				t.Fatalf("the generated sequence never failed: the property proved nothing")
			}
			continue
		}
		if !reflect.DeepEqual(got, baseline) {
			t.Fatalf("run %d diverged from the baseline run", run)
		}
	}
}

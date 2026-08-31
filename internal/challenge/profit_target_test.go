package challenge_test

import (
	"errors"
	"reflect"
	"testing"

	"praxis/internal/challenge"
	"praxis/internal/market"
)

const profitTargetCts = 100_000 // $1,000

func withTarget(t *testing.T) *challenge.Challenge {
	t.Helper()
	c, err := challenge.New(challenge.Rules{
		StartingBalanceCts: startingBalanceCts,
		MaxDailyLossCts:    dailyLossCts,
		ProfitTargetCts:    profitTargetCts,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// Scenario: reaching the profit target passes the evaluation
//
//	Given an active challenge started at $50,000 with a $1,000 target
//	When equity reaches $51,000
//	Then the challenge is passed. Reaching exactly the target is enough;
//	  one cent short is not.
func TestProfitTarget(t *testing.T) {
	tests := []struct {
		name      string
		equityCts market.Cents
		wantState challenge.State
		wantGain  market.Cents
	}{
		{"one cent short of the target", 5_099_999, challenge.StateActive, 0},
		{"exactly at the target", 5_100_000, challenge.StatePassed, 100_000},
		{"past the target", 5_200_000, challenge.StatePassed, 200_000},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := withTarget(t)
			mustOpen(t, c, opened(1, "s1", 5_000_000))
			events := mustObserve(t, c, snap(2, "s1", tc.equityCts))

			if c.State() != tc.wantState {
				t.Fatalf("state: got %v, want %v", c.State(), tc.wantState)
			}
			if tc.wantState != challenge.StatePassed {
				if len(events) != 0 {
					t.Fatalf("events: got %+v, want none", events)
				}
				return
			}
			want := []challenge.Event{{
				Time: 2_000_000_000, Sequence: 2,
				Decision: challenge.Decision{
					Kind: challenge.ChallengePassed, SessionID: "s1",
					BalanceCts: tc.equityCts, EquityCts: tc.equityCts,
					GainCts: tc.wantGain,
				},
			}}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("events\n got: %+v\nwant: %+v", events, want)
			}
		})
	}
}

// Scenario: the target is measured from where the evaluation began, not from
// where today began
//
//	Given a challenge started at $50,000 that has since run up to $50,900
//	When a new session opens at that higher equity
//	Then the target is still $51,000 in absolute terms, and a further $100
//	  reaches it — the session boundary re-bases the daily limit, never the
//	  target.
func TestANewSessionDoesNotMoveTheProfitTarget(t *testing.T) {
	c := withTarget(t)
	mustOpen(t, c, opened(1, "day-1", 5_000_000))
	mustObserve(t, c, snap(2, "day-1", 5_090_000))
	if c.State() != challenge.StateActive {
		t.Fatalf("state: got %v, want active", c.State())
	}

	mustOpen(t, c, opened(3, "day-2", 5_090_000))
	if c.StartingBalanceCts() != 5_000_000 {
		t.Fatalf("starting balance moved to %d", c.StartingBalanceCts())
	}
	if c.ReferenceEquityCts() != 5_090_000 {
		t.Fatalf("session reference: got %d, want 5090000", c.ReferenceEquityCts())
	}

	mustObserve(t, c, snap(4, "day-2", 5_100_000))
	if c.State() != challenge.StatePassed {
		t.Fatalf("state: got %v, want passed", c.State())
	}
}

// Scenario: a passed evaluation is terminal
func TestAPassedChallengeIsTerminal(t *testing.T) {
	c := withTarget(t)
	mustOpen(t, c, opened(1, "s1", 5_000_000))
	mustObserve(t, c, snap(2, "s1", 5_200_000))
	if c.State() != challenge.StatePassed {
		t.Fatalf("state: got %v, want passed", c.State())
	}

	if _, err := c.Observe(snap(3, "s1", 1_000_000)); !errors.Is(err, challenge.ErrTerminal) {
		t.Fatalf("collapsing observation: got %v, want %v", err, challenge.ErrTerminal)
	}
	if _, err := c.OpenSession(opened(4, "s2", 1_000_000)); !errors.Is(err, challenge.ErrTerminal) {
		t.Fatalf("new session: got %v, want %v", err, challenge.ErrTerminal)
	}
	if c.State() != challenge.StatePassed {
		t.Fatalf("a passed challenge changed to %v", c.State())
	}
}

// Scenario: both rules breach on the same snapshot, and the loss wins
//
//	Given a $50,000 challenge with a $1,000 target and a $1,000 daily limit,
//	  whose second session opens after a run up to $53,000
//	When equity falls to $51,500
//	Then the day is down $1,500 — past the daily limit — while the
//	  evaluation is up $1,500 from where it started, past the target.
//	The challenge fails.
//
// This state is reachable with entirely coherent rules: the two are measured
// against different references, the session's and the evaluation's. It is not
// an arithmetic impossibility and cannot be left undefined. Failing wins,
// because a simulator must never resolve an ambiguity in the trader's favour.
func TestADailyLossBreachBeatsTheProfitTarget(t *testing.T) {
	c := withTarget(t)
	mustOpen(t, c, opened(1, "day-1", 5_000_000))
	mustOpen(t, c, opened(2, "day-2", 5_300_000))

	events := mustObserve(t, c, snap(3, "day-2", 5_150_000))

	if c.State() != challenge.StateFailed {
		t.Fatalf("state: got %v, want failed", c.State())
	}
	if c.FailureReason() != challenge.FailureDailyLoss {
		t.Fatalf("reason: got %v, want daily loss", c.FailureReason())
	}
	if len(events) != 1 || events[0].Kind != challenge.ChallengeFailed {
		t.Fatalf("events: got %+v, want only a failure", events)
	}
}

// A challenge configured with no profit target can only be failed. Zero means
// unset, as it does for order prices.
func TestWithoutATargetTheChallengeNeverPasses(t *testing.T) {
	c := newChallenge(t)
	mustOpen(t, c, opened(1, "s1", 5_000_000))
	mustObserve(t, c, snap(2, "s1", 500_000_000))

	if c.State() != challenge.StateActive {
		t.Fatalf("state: got %v, want active with no target configured", c.State())
	}
}

func TestRejectsANegativeProfitTarget(t *testing.T) {
	_, err := challenge.New(challenge.Rules{
		StartingBalanceCts: startingBalanceCts, MaxDailyLossCts: dailyLossCts, ProfitTargetCts: -1,
	})
	if !errors.Is(err, challenge.ErrNegativeProfitTarget) {
		t.Fatalf("error: got %v, want %v", err, challenge.ErrNegativeProfitTarget)
	}
}

// Property: a sequence that ends passed replays identically.
func TestPropertyPassingReplayIsDeterministic(t *testing.T) {
	const runs = 20

	type outcome struct {
		state  challenge.State
		events []challenge.Event
	}

	var baseline outcome
	for run := 0; run < runs; run++ {
		c := withTarget(t)
		var events []challenge.Event

		equity := market.Cents(5_000_000)
		seq := uint64(0)
		for day := 0; day < 4; day++ {
			seq++
			id := challenge.SessionID([]string{"d1", "d2", "d3", "d4"}[day])
			ev, err := c.OpenSession(opened(seq, id, equity))
			if err != nil {
				break
			}
			events = append(events, ev...)

			for step := 0; step < 5; step++ {
				seq++
				equity += 15_000
				ev, err := c.Observe(snap(seq, id, equity))
				if err != nil {
					break
				}
				events = append(events, ev...)
			}
		}

		got := outcome{c.State(), events}
		if run == 0 {
			baseline = got
			if got.state != challenge.StatePassed {
				t.Fatalf("the generated sequence never passed: the property proved nothing")
			}
			continue
		}
		if !reflect.DeepEqual(got, baseline) {
			t.Fatalf("run %d diverged from the baseline run", run)
		}
	}
}

// Scenario: each rule reads the value it means
//
//	Given a snapshot whose balance and equity differ
//	When the target is met on equity alone, the challenge stays active,
//	  because only realised money counts toward passing
//	And when the daily limit is breached on equity alone, it fails, because
//	  an open loss must be able to end an evaluation immediately.
func TestTheRulesReadDifferentValues(t *testing.T) {
	t.Run("an open gain does not reach the target", func(t *testing.T) {
		c := withTarget(t)
		mustOpen(t, c, opened(1, "s1", 5_000_000))

		mustObserve(t, c, snapAt(2, "s1", 5_000_000, 5_200_000))
		if c.State() != challenge.StateActive {
			t.Fatalf("state: got %v, want active — the gain is unrealised", c.State())
		}
	})

	t.Run("realising that gain reaches the target", func(t *testing.T) {
		c := withTarget(t)
		mustOpen(t, c, opened(1, "s1", 5_000_000))
		mustObserve(t, c, snapAt(2, "s1", 5_000_000, 5_200_000))

		mustObserve(t, c, snap(3, "s1", 5_200_000))
		if c.State() != challenge.StatePassed {
			t.Fatalf("state: got %v, want passed once the gain is realised", c.State())
		}
	})

	t.Run("an open loss breaches the daily limit", func(t *testing.T) {
		c := withTarget(t)
		mustOpen(t, c, opened(1, "s1", 5_000_000))

		mustObserve(t, c, snapAt(2, "s1", 5_000_000, 4_899_999))
		if c.State() != challenge.StateFailed {
			t.Fatalf("state: got %v, want failed — the loss is unrealised but real", c.State())
		}
	})
}

// Scenario: a gain that is given back never passed in the first place
//
//	Given an open position whose unrealised gain passes the target level
//	When the position gives the gain back without ever being closed
//	Then the challenge was never passed. A momentary swing must not buy an
//	  irreversible approval.
func TestAnUnrealisedGainGivenBackNeverPasses(t *testing.T) {
	c := withTarget(t)
	mustOpen(t, c, opened(1, "s1", 5_000_000))

	mustObserve(t, c, snapAt(2, "s1", 5_000_000, 5_300_000))
	mustObserve(t, c, snapAt(3, "s1", 5_000_000, 5_150_000))
	mustObserve(t, c, snapAt(4, "s1", 5_000_000, 5_000_000))

	if c.State() != challenge.StateActive {
		t.Fatalf("state: got %v, want active throughout", c.State())
	}
}

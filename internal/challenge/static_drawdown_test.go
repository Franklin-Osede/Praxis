package challenge_test

import (
	"errors"
	"math"
	"reflect"
	"testing"

	"praxis/internal/challenge"
	"praxis/internal/market"
)

const maxTotalLossCts = 200_000 // $2,000, so the floor is $48,000

func withFloor(t *testing.T, dailyLossCts market.Cents) *challenge.Challenge {
	t.Helper()
	c, err := challenge.New(challenge.Rules{
		StartingBalanceCts: startingBalanceCts,
		MaxDailyLossCts:    dailyLossCts,
		MaxTotalLossCts:    maxTotalLossCts,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.StaticFloorCts() != 4_800_000 {
		t.Fatalf("floor: got %d, want 4800000", c.StaticFloorCts())
	}
	return c
}

// Scenario: equity below the static floor fails the evaluation
//
//	Given a $50,000 evaluation with a $2,000 maximum total loss, so a floor
//	  at $48,000
//	When equity reaches exactly $48,000 the challenge is still active
//	And one cent below it, the challenge fails on the static drawdown.
//
// The descent is spread over sessions so that the daily limit is never the
// rule that fires.
func TestStaticDrawdownFloor(t *testing.T) {
	tests := []struct {
		name      string
		equityCts market.Cents
		wantState challenge.State
		wantLoss  market.Cents
	}{
		{"exactly at the floor", 4_800_000, challenge.StateActive, 0},
		{"one cent below the floor", 4_799_999, challenge.StateFailed, 200_001},
		{"far below the floor", 4_750_000, challenge.StateFailed, 250_000},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := withFloor(t, dailyLossCts)
			mustOpen(t, c, opened(1, "d1", 5_000_000))
			mustObserve(t, c, snap(2, "d1", 4_910_000))
			mustOpen(t, c, opened(3, "d2", 4_910_000))
			mustObserve(t, c, snap(4, "d2", 4_820_000))
			mustOpen(t, c, opened(5, "d3", 4_820_000))

			if c.StaticFloorCts() != 4_800_000 {
				t.Fatalf("three sessions moved the floor to %d", c.StaticFloorCts())
			}

			events := mustObserve(t, c, snap(6, "d3", tc.equityCts))

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
				Kind: challenge.ChallengeFailed, Time: 6_000_000_000, Sequence: 6,
				SessionID: "d3", BalanceCts: tc.equityCts, EquityCts: tc.equityCts,
				LossCts: tc.wantLoss, Reason: challenge.FailureStaticDrawdown,
			}}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("events\n got: %+v\nwant: %+v", events, want)
			}
		})
	}
}

// Scenario: earlier profit does not lift the floor
//
//	Given an evaluation that has run up to $55,000
//	When equity falls back below $48,000
//	Then it fails, because the floor is anchored to the configured starting
//	  balance and nothing moves it.
func TestEarlierProfitDoesNotLiftTheFloor(t *testing.T) {
	c := withFloor(t, 1_000_000)
	mustOpen(t, c, opened(1, "d1", 5_000_000))
	mustObserve(t, c, snap(2, "d1", 5_500_000))

	if c.StaticFloorCts() != 4_800_000 {
		t.Fatalf("a profit lifted the floor to %d", c.StaticFloorCts())
	}

	mustObserve(t, c, snap(3, "d1", 4_799_999))
	if c.State() != challenge.StateFailed || c.FailureReason() != challenge.FailureStaticDrawdown {
		t.Fatalf("got %v %v, want failed on static drawdown", c.State(), c.FailureReason())
	}
}

// Scenario: when both loss rules breach, the daily limit is the reason
//
// The outcome is the same either way; the precedence exists so that a recorded
// reason is stable rather than incidental.
func TestDailyLossKeepsPrecedenceOverTheStaticFloor(t *testing.T) {
	c := withFloor(t, dailyLossCts)
	mustOpen(t, c, opened(1, "d1", 5_000_000))

	mustObserve(t, c, snap(2, "d1", 4_700_000))

	if c.State() != challenge.StateFailed {
		t.Fatalf("state: got %v, want failed", c.State())
	}
	if c.FailureReason() != challenge.FailureDailyLoss {
		t.Fatalf("reason: got %v, want daily loss", c.FailureReason())
	}
}

// A zero maximum total loss configures no floor at all.
func TestWithoutATotalLossThereIsNoFloor(t *testing.T) {
	c := newChallenge(t)
	if c.StaticFloorCts() != 0 {
		t.Fatalf("floor: got %d, want none", c.StaticFloorCts())
	}
	mustOpen(t, c, opened(1, "d1", 5_000_000))
	mustObserve(t, c, snap(2, "d1", 4_950_000))
	mustOpen(t, c, opened(3, "d2", 4_950_000))
	mustObserve(t, c, snap(4, "d2", 1_000))

	if c.State() != challenge.StateFailed || c.FailureReason() != challenge.FailureDailyLoss {
		t.Fatalf("got %v %v, want the daily limit to be the only rule", c.State(), c.FailureReason())
	}
}

// Scenario: an evaluation must activate at the valuation it was contracted to
//
// This does not prove the account holds no position: a position at break-even
// has zero unrealised P&L and would satisfy it. It proves only that the
// evaluation began where its rules say it began.
func TestActivationRequiresTheConfiguredStartingValuation(t *testing.T) {
	tests := []struct {
		name       string
		balanceCts market.Cents
		equityCts  market.Cents
	}{
		{"balance below the configured start", 4_999_999, 5_000_000},
		{"balance above the configured start", 5_000_001, 5_000_000},
		{"an open position at activation", 5_000_000, 4_999_000},
		{"both figures wrong", 4_000_000, 4_000_000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := withFloor(t, dailyLossCts)
			got, err := c.OpenSession(openedAt(1, "d1", tc.balanceCts, tc.equityCts))
			if !errors.Is(err, challenge.ErrNotStartingValuation) {
				t.Fatalf("error: got %v, want %v", err, challenge.ErrNotStartingValuation)
			}
			if got != nil {
				t.Fatalf("rejected activation produced events: %+v", got)
			}
			if c.State() != challenge.StatePending || c.SessionID() != "" {
				t.Fatalf("a rejected activation mutated the challenge: %v %v", c.State(), c.SessionID())
			}
		})
	}

	// A later session is free to open anywhere; only activation is pinned.
	c := withFloor(t, dailyLossCts)
	mustOpen(t, c, opened(1, "d1", 5_000_000))
	mustOpen(t, c, openedAt(2, "d2", 5_000_000, 4_950_000))
	if c.ReferenceEquityCts() != 4_950_000 {
		t.Fatalf("reference: got %d, want 4950000", c.ReferenceEquityCts())
	}
}

func TestRejectsInvalidDrawdownRules(t *testing.T) {
	tests := []struct {
		name  string
		rules challenge.Rules
		want  error
	}{
		{"no starting balance", challenge.Rules{MaxDailyLossCts: dailyLossCts}, challenge.ErrNonPositiveStartingBalance},
		{"negative starting balance", challenge.Rules{StartingBalanceCts: -1, MaxDailyLossCts: dailyLossCts}, challenge.ErrNonPositiveStartingBalance},
		{"negative total loss", challenge.Rules{StartingBalanceCts: startingBalanceCts, MaxDailyLossCts: dailyLossCts, MaxTotalLossCts: -1}, challenge.ErrNegativeTotalLoss},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := challenge.New(tc.rules); !errors.Is(err, tc.want) {
				t.Fatalf("error: got %v, want %v", err, tc.want)
			}
		})
	}
}

// Overflow computing the floor is unreachable: the starting balance is
// positive and the maximum total loss is non-negative, so their difference
// always fits. Overflow measuring a loss is reachable, and is rejected without
// touching the challenge.
func TestOverflowMeasuringALossIsRejected(t *testing.T) {
	c, err := challenge.New(challenge.Rules{
		StartingBalanceCts: math.MaxInt64,
		MaxDailyLossCts:    dailyLossCts,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mustOpen(t, c, opened(1, "d1", math.MaxInt64))

	got, err := c.Observe(snap(2, "d1", -1))
	if !errors.Is(err, market.ErrOverflow) {
		t.Fatalf("error: got %v, want %v", err, market.ErrOverflow)
	}
	if got != nil {
		t.Fatalf("overflowing snapshot produced events: %+v", got)
	}
	if c.State() != challenge.StateActive {
		t.Fatalf("an overflow failed the challenge")
	}
}

// Property: a sequence ending on the static floor replays identically.
func TestPropertyStaticDrawdownReplayIsDeterministic(t *testing.T) {
	const runs = 20

	type outcome struct {
		state  challenge.State
		reason challenge.FailureReason
		floor  market.Cents
		events []challenge.Event
	}

	var baseline outcome
	for run := 0; run < runs; run++ {
		c := withFloor(t, dailyLossCts)
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

			for step := 0; step < 3; step++ {
				seq++
				equity -= 30_000
				ev, err := c.Observe(snap(seq, id, equity))
				if err != nil {
					break
				}
				events = append(events, ev...)
			}
		}

		got := outcome{c.State(), c.FailureReason(), c.StaticFloorCts(), events}
		if run == 0 {
			baseline = got
			if got.reason != challenge.FailureStaticDrawdown {
				t.Fatalf("the generated sequence failed on %v, not the floor", got.reason)
			}
			continue
		}
		if !reflect.DeepEqual(got, baseline) {
			t.Fatalf("run %d diverged from the baseline run", run)
		}
	}
}

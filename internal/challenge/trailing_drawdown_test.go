package challenge_test

import (
	"errors"
	"reflect"
	"testing"

	"praxis/internal/challenge"
	"praxis/internal/market"
)

const trailingDrawdownCts = 200_000 // $2,000

func withTrailing(t *testing.T) *challenge.Challenge {
	t.Helper()
	c, err := challenge.New(challenge.Rules{
		StartingBalanceCts:  startingBalanceCts,
		MaxDailyLossCts:     1_000_000, // keep daily loss out of these tests
		TrailingDrawdownCts: trailingDrawdownCts,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func mustHighWater(t *testing.T, c *challenge.Challenge) market.Cents {
	t.Helper()
	high, ok := c.HighWater()
	if !ok {
		t.Fatal("trailing drawdown is not enabled")
	}
	return high
}

// Scenario: a trailing floor follows new equity highs and never moves down
//
// Given a $50,000 evaluation with a $2,000 trailing drawdown
// When equity reaches a new high of $53,000
// Then the high-water mark is $53,000 and the floor is $51,000
// And falling to exactly $51,000 survives, while one cent below fails.
func TestTrailingDrawdownFollowsEquityHighWater(t *testing.T) {
	c := withTrailing(t)
	mustOpen(t, c, opened(1, "d1", 5_000_000))

	if got := mustHighWater(t, c); got != 5_000_000 {
		t.Fatalf("initial high-water: got %d, want 5000000", got)
	}
	if got, ok := c.TrailingThresholdCts(); !ok || got != 4_800_000 {
		t.Fatalf("initial threshold: got %d enabled %v, want 4800000 true", got, ok)
	}

	mustObserve(t, c, snapAt(2, "d1", 5_000_000, 5_300_000))
	if got := mustHighWater(t, c); got != 5_300_000 {
		t.Fatalf("high-water: got %d, want 5300000", got)
	}
	if got, ok := c.TrailingThresholdCts(); !ok || got != 5_100_000 {
		t.Fatalf("threshold: got %d enabled %v, want 5100000 true", got, ok)
	}

	// A lower observation must not lower either value.
	mustObserve(t, c, snapAt(3, "d1", 5_000_000, 5_100_000))
	if c.State() != challenge.StateActive {
		t.Fatalf("state: got %v, want active exactly at the threshold", c.State())
	}
	if high := mustHighWater(t, c); high != 5_300_000 {
		t.Fatalf("a decline lowered high-water to %d", high)
	}

	events := mustObserve(t, c, snapAt(4, "d1", 5_000_000, 5_099_999))
	if c.State() != challenge.StateFailed || c.FailureReason() != challenge.FailureTrailingDrawdown {
		t.Fatalf("got %v %v, want failed on trailing drawdown", c.State(), c.FailureReason())
	}
	want := []challenge.Event{{
		Time: 4_000_000_000, Sequence: 4,
		Decision: challenge.Decision{
			Kind: challenge.ChallengeFailed, SessionID: "d1",
			BalanceCts: 5_000_000, EquityCts: 5_099_999,
			LossCts: 200_001, Reason: challenge.FailureTrailingDrawdown,
			HighWaterCts: 5_300_000, ThresholdCts: 5_100_000,
		},
	}}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events\n got: %+v\nwant: %+v", events, want)
	}
}

func TestTrailingDrawdownDoesNotResetAtASessionBoundary(t *testing.T) {
	c := withTrailing(t)
	mustOpen(t, c, opened(1, "d1", 5_000_000))
	mustObserve(t, c, snapAt(2, "d1", 5_000_000, 5_300_000))
	mustOpen(t, c, openedAt(3, "d2", 5_000_000, 5_200_000))

	if high := mustHighWater(t, c); high != 5_300_000 {
		t.Fatalf("new session reset high-water to %d", high)
	}
	if threshold, ok := c.TrailingThresholdCts(); !ok || threshold != 5_100_000 {
		t.Fatalf("new session moved threshold to %d enabled %v", threshold, ok)
	}

	mustObserve(t, c, snapAt(4, "d2", 5_000_000, 5_099_999))
	if c.FailureReason() != challenge.FailureTrailingDrawdown {
		t.Fatalf("reason: got %v, want trailing drawdown", c.FailureReason())
	}
}

func TestTrailingDrawdownUsesEquityNotBalance(t *testing.T) {
	c := withTrailing(t)
	mustOpen(t, c, opened(1, "d1", 5_000_000))

	// The balance never moves. An open gain raises the high-water mark, and
	// giving enough of it back breaches the resulting threshold.
	mustObserve(t, c, snapAt(2, "d1", 5_000_000, 5_300_000))
	mustObserve(t, c, snapAt(3, "d1", 5_000_000, 5_099_999))

	if c.State() != challenge.StateFailed || c.FailureReason() != challenge.FailureTrailingDrawdown {
		t.Fatalf("got %v %v, want an equity-only trailing failure", c.State(), c.FailureReason())
	}
}

func TestLossRulesKeepPrecedenceOverTrailingDrawdown(t *testing.T) {
	t.Run("daily before trailing", func(t *testing.T) {
		c, err := challenge.New(challenge.Rules{
			StartingBalanceCts: 5_000_000, MaxDailyLossCts: 100_000,
			TrailingDrawdownCts: 200_000,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		mustOpen(t, c, opened(1, "d1", 5_000_000))
		mustObserve(t, c, snap(2, "d1", 4_700_000))
		if c.FailureReason() != challenge.FailureDailyLoss {
			t.Fatalf("reason: got %v, want daily loss", c.FailureReason())
		}
	})

	t.Run("static before trailing", func(t *testing.T) {
		c, err := challenge.New(challenge.Rules{
			StartingBalanceCts: 5_000_000, MaxDailyLossCts: 1_000_000,
			MaxTotalLossCts: 100_000, TrailingDrawdownCts: 200_000,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		mustOpen(t, c, opened(1, "d1", 5_000_000))
		mustObserve(t, c, snap(2, "d1", 4_899_999))
		if c.FailureReason() != challenge.FailureStaticDrawdown {
			t.Fatalf("reason: got %v, want static drawdown", c.FailureReason())
		}
	})
}

func TestRejectedSnapshotDoesNotMoveTrailingState(t *testing.T) {
	c := withTrailing(t)
	mustOpen(t, c, opened(1, "d1", 5_000_000))
	mustObserve(t, c, snapAt(2, "d1", 5_000_000, 5_100_000))

	beforeHigh := mustHighWater(t, c)
	beforeThreshold, _ := c.TrailingThresholdCts()
	if _, err := c.Observe(snapAt(2, "d1", 5_000_000, 5_500_000)); !errors.Is(err, challenge.ErrOutOfOrder) {
		t.Fatalf("error: got %v, want %v", err, challenge.ErrOutOfOrder)
	}
	if high := mustHighWater(t, c); high != beforeHigh {
		t.Fatalf("rejected input moved high-water from %d to %d", beforeHigh, high)
	}
	if got, _ := c.TrailingThresholdCts(); got != beforeThreshold {
		t.Fatalf("rejected input moved threshold from %d to %d", beforeThreshold, got)
	}
}

func TestDrawdownEnablementDoesNotUseAZeroFloorSentinel(t *testing.T) {
	withZeroFloor, err := challenge.New(challenge.Rules{
		StartingBalanceCts: 5_000_000, MaxDailyLossCts: dailyLossCts,
		MaxTotalLossCts: 5_000_000,
	})
	if err != nil {
		t.Fatalf("New zero floor: %v", err)
	}
	if floor, ok := withZeroFloor.StaticFloor(); !ok || floor != 0 {
		t.Fatalf("real zero floor: got %d enabled %v, want 0 true", floor, ok)
	}

	disabled := newChallenge(t)
	if floor, ok := disabled.StaticFloor(); ok || floor != 0 {
		t.Fatalf("disabled static floor: got %d enabled %v, want 0 false", floor, ok)
	}
	if threshold, ok := disabled.TrailingThresholdCts(); ok || threshold != 0 {
		t.Fatalf("disabled trailing floor: got %d enabled %v, want 0 false", threshold, ok)
	}
	if high, ok := disabled.HighWater(); ok || high != 0 {
		t.Fatalf("disabled high-water: got %d enabled %v, want 0 false", high, ok)
	}
}

func TestRejectsNegativeTrailingDrawdown(t *testing.T) {
	_, err := challenge.New(challenge.Rules{
		StartingBalanceCts:  5_000_000,
		MaxDailyLossCts:     dailyLossCts,
		TrailingDrawdownCts: -1,
	})
	if !errors.Is(err, challenge.ErrNegativeTrailingDrawdown) {
		t.Fatalf("error: got %v, want %v", err, challenge.ErrNegativeTrailingDrawdown)
	}
}

func TestPropertyTrailingDrawdownReplayIsDeterministic(t *testing.T) {
	const runs = 20
	type outcome struct {
		state     challenge.State
		reason    challenge.FailureReason
		high      market.Cents
		threshold market.Cents
		events    []challenge.Event
	}

	var baseline outcome
	for run := 0; run < runs; run++ {
		c := withTrailing(t)
		var events []challenge.Event
		ev, err := c.OpenSession(opened(1, "d1", 5_000_000))
		if err != nil {
			t.Fatalf("run %d OpenSession: %v", run, err)
		}
		events = append(events, ev...)

		values := []market.Cents{5_050_000, 5_300_000, 5_250_000, 5_100_000, 5_099_999}
		previousHigh := mustHighWater(t, c)
		previousThreshold, _ := c.TrailingThresholdCts()
		for i, equity := range values {
			ev, err = c.Observe(snapAt(uint64(i+2), "d1", 5_000_000, equity))
			if err != nil {
				t.Fatalf("run %d step %d: %v", run, i, err)
			}
			events = append(events, ev...)
			if mustHighWater(t, c) < previousHigh {
				t.Fatalf("run %d: high-water moved down", run)
			}
			threshold, _ := c.TrailingThresholdCts()
			if threshold < previousThreshold {
				t.Fatalf("run %d: threshold moved down", run)
			}
			previousHigh, previousThreshold = mustHighWater(t, c), threshold
		}

		threshold, _ := c.TrailingThresholdCts()
		got := outcome{c.State(), c.FailureReason(), mustHighWater(t, c), threshold, events}
		if run == 0 {
			baseline = got
			if got.reason != challenge.FailureTrailingDrawdown {
				t.Fatalf("sequence ended on %v, not trailing drawdown", got.reason)
			}
			continue
		}
		if !reflect.DeepEqual(got, baseline) {
			t.Fatalf("run %d diverged from baseline", run)
		}
	}
}

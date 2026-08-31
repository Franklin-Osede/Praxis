package session_test

import (
	"errors"
	"testing"

	"praxis/internal/challenge"
	"praxis/internal/market"
	"praxis/internal/portfolio"
	"praxis/internal/session"
)

// tamper returns the script's events with one of them replaced.
func tamper(t *testing.T, replace func(session.Event) (session.Event, bool)) []session.Event {
	t.Helper()
	events := script(t).Events()
	for n, e := range events {
		if got, ok := replace(e); ok {
			events[n] = got
			return events
		}
	}
	t.Fatal("nothing to tamper with")
	return nil
}

// Scenario: a journal is not believed, it is proved
//
//	Given a log whose derived facts have been altered
//	When it is replayed
//	Then each alteration is rejected, because every derived fact is rebuilt
//	  from the facts that caused it and compared exactly.
//
// A log that merely reads consistently is not enough: a fabricated equity
// could record an evaluation failing where no account could have failed.
func TestReplayRefusesFabricatedFacts(t *testing.T) {
	tests := []struct {
		name    string
		replace func(session.Event) (session.Event, bool)
		want    error
	}{
		{
			// A single cent, too small to trip any rule. A larger lie would
			// change a decision and be caught by that comparison instead,
			// proving nothing about the valuation check.
			name: "an equity the account never held",
			replace: func(e session.Event) (session.Event, bool) {
				v, ok := e.(session.AccountValued)
				if !ok {
					return nil, false
				}
				v.EquityCts--
				return v, true
			},
			want: session.ErrFabricated,
		},
		{
			name: "a balance the account never held",
			replace: func(e session.Event) (session.Event, bool) {
				v, ok := e.(session.AccountValued)
				if !ok {
					return nil, false
				}
				v.BalanceCts += 1
				return v, true
			},
			want: session.ErrFabricated,
		},
		{
			name: "a realised amount the fill did not produce",
			replace: func(e session.Event) (session.Event, bool) {
				c, ok := e.(session.PositionChanged)
				if !ok || c.Change.RealisedCts == 0 {
					return nil, false
				}
				c.Change.RealisedCts = -c.Change.RealisedCts
				return c, true
			},
			want: session.ErrFabricated,
		},
		{
			name: "a commission the account did not charge",
			replace: func(e session.Event) (session.Event, bool) {
				c, ok := e.(session.PositionChanged)
				if !ok {
					return nil, false
				}
				c.Change.FeeCts = 0
				return c, true
			},
			want: session.ErrFabricated,
		},
		{
			name: "a decision the evaluation did not make",
			replace: func(e session.Event) (session.Event, bool) {
				d, ok := e.(session.ChallengeDecision)
				if !ok {
					return nil, false
				}
				d.Decision.Kind = challenge.ChallengeFailed
				d.Decision.Reason = challenge.FailureDailyLoss
				return d, true
			},
			want: session.ErrFabricated,
		},
		{
			name: "a decision blamed on the wrong cause",
			replace: func(e session.Event) (session.Event, bool) {
				d, ok := e.(session.ChallengeDecision)
				if !ok {
					return nil, false
				}
				d.CausedBySequence++
				return d, true
			},
			want: session.ErrFabricated,
		},
		{
			name: "a fill the account was never given",
			replace: func(e session.Event) (session.Event, bool) {
				f, ok := e.(session.FillProduced)
				if !ok {
					return nil, false
				}
				f.Fill.Qty++
				return f, true
			},
			want: session.ErrFabricated,
		},
		{
			name: "an event tagged as something it is not",
			replace: func(e session.Event) (session.Event, bool) {
				o, ok := e.(session.MarketObserved)
				if !ok {
					return nil, false
				}
				o.Kind = session.KindSessionEnded
				return o, true
			},
			want: session.ErrStructure,
		},
		{
			name: "time running backwards",
			replace: func(e session.Event) (session.Event, bool) {
				o, ok := e.(session.MarketObserved)
				if !ok {
					return nil, false
				}
				o.Time = 1
				return o, true
			},
			want: session.ErrStructure,
		},
		{
			name: "a valuation attributed to another session",
			replace: func(e session.Event) (session.Event, bool) {
				v, ok := e.(session.AccountValued)
				if !ok {
					return nil, false
				}
				v.SessionID = "somewhere-else"
				return v, true
			},
			want: session.ErrStructure,
		},
		{
			name: "a session ended under the wrong identifier",
			replace: func(e session.Event) (session.Event, bool) {
				v, ok := e.(session.SessionEnded)
				if !ok {
					return nil, false
				}
				v.SessionID = "somewhere-else"
				return v, true
			},
			want: session.ErrStructure,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := session.Replay(tamper(t, tc.replace)); !errors.Is(err, tc.want) {
				t.Fatalf("error: got %v, want %v", err, tc.want)
			}
		})
	}
}

// Scenario: an internally coherent lie is still a lie
//
//	Given a log whose realised P&L was altered and whose every later context
//	  was adjusted to agree with it
//	When Verify is asked
//	Then it accepts, because the log agrees with itself
//	And when Replay is asked, it refuses, because the account that produced
//	  the fill never realised that amount.
//
// This is the whole argument for replaying against the aggregates rather than
// checking the log against itself.
func TestVerifyAcceptsACoherentLieThatReplayRefuses(t *testing.T) {
	events := script(t).Events()

	// The forgery is adjusted for only within the trading session it happened
	// in, because the counters reset at a boundary. A liar has to keep the
	// story straight to the same standard the log does.
	const forged = market.Cents(1_000)
	var delta market.Cents
	done := false
	for n, e := range events {
		if done {
			break
		}
		switch v := e.(type) {
		case session.PositionChanged:
			if v.Change.RealisedCts == 0 || delta != 0 {
				continue
			}
			delta = forged - v.Change.RealisedCts
			v.Change.RealisedCts = forged
			events[n] = v
		case session.OrderSubmitted:
			if delta == 0 {
				continue
			}
			v.Context.SessionRealisedCts += delta
			events[n] = v
		case session.SessionOpened:
			done = delta != 0
		}
	}
	if delta == 0 {
		t.Fatal("the script realised nothing to forge")
	}

	if err := session.Verify(events); err != nil {
		t.Fatalf("Verify: got %v, want the forged log to look coherent", err)
	}
	if _, err := session.Replay(events); !errors.Is(err, session.ErrFabricated) {
		t.Fatalf("Replay: got %v, want %v", err, session.ErrFabricated)
	}
}

// Every position change a fill produced must be recorded, and no more.
func TestReplayRefusesAFillWhoseChangesAreMissing(t *testing.T) {
	events := script(t).Events()

	var pruned []session.Event
	dropped := false
	for _, e := range events {
		if _, ok := e.(session.PositionChanged); ok && !dropped {
			dropped = true
			continue
		}
		pruned = append(pruned, e)
	}
	if !dropped {
		t.Fatal("the script produced no position change")
	}

	// Renumber so the missing event is not caught as a sequence gap instead.
	if _, err := session.Replay(renumber(pruned)); !errors.Is(err, session.ErrFabricated) {
		t.Fatalf("error: got %v, want %v", err, session.ErrFabricated)
	}
}

// A decision that was never made cannot be inserted.
func TestReplayRefusesAnInventedDecision(t *testing.T) {
	events := script(t).Events()

	var extended []session.Event
	for _, e := range events {
		extended = append(extended, e)
		if v, ok := e.(session.AccountValued); ok && len(extended) > 5 {
			extended = append(extended, session.ChallengeDecision{
				Envelope: session.Envelope{Time: v.Time, Kind: session.KindChallengeDecision},
				Decision: challenge.Decision{Kind: challenge.ChallengeFailed, SessionID: v.SessionID},
			})
			break
		}
	}
	extended = append(extended, events[len(extended)-1:]...)

	if _, err := session.Replay(renumber(extended)); !errors.Is(err, session.ErrFabricated) {
		t.Fatalf("error: got %v, want %v", err, session.ErrFabricated)
	}
}

// renumber rewrites sequences so a structural test is not caught by the
// contiguity check first.
func renumber(events []session.Event) []session.Event {
	out := make([]session.Event, 0, len(events))
	for n, e := range events {
		seq := uint64(n + 1)
		switch v := e.(type) {
		case session.SessionStarted:
			v.Sequence = seq
			out = append(out, v)
		case session.SessionOpened:
			v.Sequence = seq
			out = append(out, v)
		case session.MarketObserved:
			v.Sequence = seq
			out = append(out, v)
		case session.OrderSubmitted:
			v.Sequence = seq
			out = append(out, v)
		case session.FillProduced:
			v.Sequence = seq
			out = append(out, v)
		case session.PositionChanged:
			v.Sequence = seq
			out = append(out, v)
		case session.AccountValued:
			v.Sequence = seq
			out = append(out, v)
		case session.ChallengeDecision:
			v.Sequence = seq
			out = append(out, v)
		case session.SessionEnded:
			v.Sequence = seq
			out = append(out, v)
		}
	}
	return out
}

var _ = portfolio.PositionEvent{}

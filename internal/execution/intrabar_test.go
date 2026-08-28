package execution_test

import (
	"errors"
	"math/rand"
	"reflect"
	"testing"

	"praxis/internal/execution"
	"praxis/internal/market"
	"praxis/internal/portfolio"
)

func bar(open, high, low, close market.Ticks) market.Bar {
	return market.Bar{
		Instrument: mnq,
		StartTime:  1_700_000_000_000_000_000,
		EndTime:    1_700_000_060_000_000_000,
		Open:       open,
		High:       high,
		Low:        low,
		Close:      close,
		Sequence:   1,
	}
}

var (
	long  = portfolio.Position{Instrument: mnq, NetQty: 2, CostBasisCts: 2_000_000}
	short = portfolio.Position{Instrument: mnq, NetQty: -2, CostBasisCts: -2_000_000}
)

// Scenario: a completed bar resolves a protected position against the trader
//
//	Given a position with a stop and a target
//	When a bar reaches both levels without saying in which order
//	Then the stop wins and the ambiguity is recorded,
//	  and when the bar opens beyond a level the fill is the open, not the
//	  level, because the open is the first price the bar actually showed.
func TestWorstCaseIntrabarResolve(t *testing.T) {
	longLevels := portfolio.ProtectiveLevels{Stop: 19_990, Target: 20_020}
	shortLevels := portfolio.ProtectiveLevels{Stop: 20_020, Target: 19_990}

	tests := []struct {
		name   string
		bar    market.Bar
		pos    portfolio.Position
		levels portfolio.ProtectiveLevels
		want   execution.IntrabarResult
	}{
		{
			name:   "long: neither level is reached",
			bar:    bar(20_000, 20_010, 19_995, 20_005),
			pos:    long,
			levels: longLevels,
			want:   execution.IntrabarResult{Outcome: execution.IntrabarNone},
		},
		{
			name:   "long: only the stop is reached",
			bar:    bar(20_000, 20_010, 19_985, 19_990),
			pos:    long,
			levels: longLevels,
			want:   execution.IntrabarResult{Outcome: execution.IntrabarStop, Price: 19_990},
		},
		{
			name:   "long: only the target is reached",
			bar:    bar(20_000, 20_025, 19_995, 20_020),
			pos:    long,
			levels: longLevels,
			want:   execution.IntrabarResult{Outcome: execution.IntrabarTarget, Price: 20_020},
		},
		{
			name:   "long: both reached, the stop wins and the bar is ambiguous",
			bar:    bar(20_000, 20_025, 19_985, 20_010),
			pos:    long,
			levels: longLevels,
			want:   execution.IntrabarResult{Outcome: execution.IntrabarStop, Price: 19_990, Ambiguous: true},
		},
		{
			name:   "long: an adverse gap fills at the open, worse than the stop",
			bar:    bar(19_950, 19_960, 19_940, 19_945),
			pos:    long,
			levels: longLevels,
			want:   execution.IntrabarResult{Outcome: execution.IntrabarStop, Price: 19_950, Gapped: true},
		},
		{
			name:   "long: a gap through the stop settles the order, so both reached is not ambiguous",
			bar:    bar(19_950, 20_025, 19_940, 20_020),
			pos:    long,
			levels: longLevels,
			want:   execution.IntrabarResult{Outcome: execution.IntrabarStop, Price: 19_950, Gapped: true},
		},
		{
			name:   "long: a gap through the target still fills at the target, never better",
			bar:    bar(20_030, 20_040, 19_985, 20_035),
			pos:    long,
			levels: longLevels,
			want:   execution.IntrabarResult{Outcome: execution.IntrabarTarget, Price: 20_020, Gapped: true},
		},
		{
			name:   "long: the stop exactly at the low counts as reached",
			bar:    bar(20_000, 20_010, 19_990, 20_000),
			pos:    long,
			levels: longLevels,
			want:   execution.IntrabarResult{Outcome: execution.IntrabarStop, Price: 19_990},
		},
		{
			name:   "long: an open exactly at the stop is reached but has not gapped",
			bar:    bar(19_990, 20_010, 19_985, 20_000),
			pos:    long,
			levels: longLevels,
			want:   execution.IntrabarResult{Outcome: execution.IntrabarStop, Price: 19_990},
		},
		{
			name:   "long: a stop with no target set",
			bar:    bar(20_000, 20_030, 19_985, 20_025),
			pos:    long,
			levels: portfolio.ProtectiveLevels{Stop: 19_990},
			want:   execution.IntrabarResult{Outcome: execution.IntrabarStop, Price: 19_990},
		},
		{
			name:   "long: a target with no stop set",
			bar:    bar(20_000, 20_025, 19_900, 20_020),
			pos:    long,
			levels: portfolio.ProtectiveLevels{Target: 20_020},
			want:   execution.IntrabarResult{Outcome: execution.IntrabarTarget, Price: 20_020},
		},
		{
			name:   "long: no levels at all",
			bar:    bar(20_000, 20_100, 19_900, 20_050),
			pos:    long,
			levels: portfolio.ProtectiveLevels{},
			want:   execution.IntrabarResult{Outcome: execution.IntrabarNone},
		},
		{
			name:   "short: neither level is reached",
			bar:    bar(20_000, 20_010, 19_995, 20_005),
			pos:    short,
			levels: shortLevels,
			want:   execution.IntrabarResult{Outcome: execution.IntrabarNone},
		},
		{
			name:   "short: only the stop is reached",
			bar:    bar(20_000, 20_025, 19_995, 20_020),
			pos:    short,
			levels: shortLevels,
			want:   execution.IntrabarResult{Outcome: execution.IntrabarStop, Price: 20_020},
		},
		{
			name:   "short: only the target is reached",
			bar:    bar(20_000, 20_010, 19_985, 19_990),
			pos:    short,
			levels: shortLevels,
			want:   execution.IntrabarResult{Outcome: execution.IntrabarTarget, Price: 19_990},
		},
		{
			name:   "short: both reached, the stop wins and the bar is ambiguous",
			bar:    bar(20_000, 20_025, 19_985, 20_010),
			pos:    short,
			levels: shortLevels,
			want:   execution.IntrabarResult{Outcome: execution.IntrabarStop, Price: 20_020, Ambiguous: true},
		},
		{
			name:   "short: an adverse gap fills at the open, worse than the stop",
			bar:    bar(20_050, 20_060, 20_040, 20_055),
			pos:    short,
			levels: shortLevels,
			want:   execution.IntrabarResult{Outcome: execution.IntrabarStop, Price: 20_050, Gapped: true},
		},
		{
			name:   "short: a gap through the target still fills at the target, never better",
			bar:    bar(19_980, 20_025, 19_970, 19_975),
			pos:    short,
			levels: shortLevels,
			want:   execution.IntrabarResult{Outcome: execution.IntrabarTarget, Price: 19_990, Gapped: true},
		},
	}

	var policy execution.WorstCaseIntrabar
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := policy.Resolve(tc.bar, tc.pos, tc.levels)
			if err != nil {
				t.Fatalf("Resolve returned error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("result\n got: %+v\nwant: %+v", got, tc.want)
			}
		})
	}
}

// Scenario: input that cannot describe a real bar resolution is rejected
func TestWorstCaseIntrabarRejectsImpossibleInput(t *testing.T) {
	goodBar := bar(20_000, 20_010, 19_990, 20_005)
	levels := portfolio.ProtectiveLevels{Stop: 19_990, Target: 20_020}

	tests := []struct {
		name      string
		bar       market.Bar
		pos       portfolio.Position
		levels    portfolio.ProtectiveLevels
		wantClass error
		wantCause error
	}{
		{
			name:      "bar with an empty interval",
			bar:       market.Bar{Instrument: mnq, StartTime: 5, EndTime: 5, Open: 1, High: 1, Low: 1, Close: 1},
			pos:       long,
			levels:    levels,
			wantClass: execution.ErrInvalidBar,
			wantCause: market.ErrEmptyInterval,
		},
		{
			name:      "bar whose open is outside its range",
			bar:       bar(20_050, 20_010, 19_990, 20_005),
			pos:       long,
			levels:    levels,
			wantClass: execution.ErrInvalidBar,
			wantCause: market.ErrOpenOutsideRange,
		},
		{
			name:      "bar whose close is outside its range",
			bar:       bar(20_000, 20_010, 19_990, 19_900),
			pos:       long,
			levels:    levels,
			wantClass: execution.ErrInvalidBar,
			wantCause: market.ErrCloseOutsideRange,
		},
		{
			name:      "bar with negative volume",
			bar:       market.Bar{Instrument: mnq, StartTime: 1, EndTime: 2, Open: 20_000, High: 20_010, Low: 19_990, Close: 20_000, Volume: -1},
			pos:       long,
			levels:    levels,
			wantClass: execution.ErrInvalidBar,
			wantCause: market.ErrNegativeVolume,
		},
		{
			name:      "bar without an instrument",
			bar:       market.Bar{StartTime: 1, EndTime: 2, Open: 20_000, High: 20_010, Low: 19_990, Close: 20_000},
			pos:       long,
			levels:    levels,
			wantClass: execution.ErrInvalidBar,
			wantCause: market.ErrEmptySymbol,
		},
		{
			name:      "position without an instrument",
			bar:       goodBar,
			pos:       portfolio.Position{NetQty: 2, CostBasisCts: 1},
			levels:    levels,
			wantClass: execution.ErrInvalidPosition,
			wantCause: market.ErrEmptySymbol,
		},
		{
			name:      "position in another instrument",
			bar:       goodBar,
			pos:       portfolio.Position{Instrument: market.Instrument{Symbol: "MES", CentsPerTick: 125}, NetQty: 2, CostBasisCts: 2_000_000},
			levels:    levels,
			wantClass: execution.ErrInstrumentMismatch,
			wantCause: execution.ErrInstrumentMismatch,
		},
		{
			name:      "protective levels on a flat position",
			bar:       goodBar,
			pos:       portfolio.Position{Instrument: mnq},
			levels:    levels,
			wantClass: execution.ErrInvalidLevels,
			wantCause: portfolio.ErrLevelsOnFlatPosition,
		},
		{
			name:      "a long whose stop is above its target",
			bar:       goodBar,
			pos:       long,
			levels:    portfolio.ProtectiveLevels{Stop: 20_020, Target: 19_990},
			wantClass: execution.ErrInvalidLevels,
			wantCause: portfolio.ErrInvertedLevels,
		},
		{
			name:      "a short whose stop is below its target",
			bar:       goodBar,
			pos:       short,
			levels:    portfolio.ProtectiveLevels{Stop: 19_990, Target: 20_020},
			wantClass: execution.ErrInvalidLevels,
			wantCause: portfolio.ErrInvertedLevels,
		},
	}

	var policy execution.WorstCaseIntrabar
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := policy.Resolve(tc.bar, tc.pos, tc.levels)
			if !errors.Is(err, tc.wantClass) {
				t.Fatalf("error class: got %v, want %v", err, tc.wantClass)
			}
			if !errors.Is(err, tc.wantCause) {
				t.Fatalf("error cause: got %v, want %v", err, tc.wantCause)
			}
			if got != (execution.IntrabarResult{}) {
				t.Fatalf("rejected input produced a result: %+v", got)
			}
		})
	}
}

// A flat position with no levels is legal input and resolves to nothing.
func TestWorstCaseIntrabarFlatPositionWithoutLevels(t *testing.T) {
	var policy execution.WorstCaseIntrabar
	got, err := policy.Resolve(bar(20_000, 20_010, 19_990, 20_005), portfolio.Position{Instrument: mnq}, portfolio.ProtectiveLevels{})
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if got.Outcome != execution.IntrabarNone {
		t.Fatalf("outcome: got %v, want none", got.Outcome)
	}
}

// randomProtectedBar builds a valid bar around a protected position from an
// explicitly seeded source.
func randomProtectedBar(r *rand.Rand) (market.Bar, portfolio.Position, portfolio.ProtectiveLevels) {
	mid := market.Ticks(r.Int63n(2_000) + 19_000)
	spread := market.Ticks(r.Int63n(60) + 1)

	open := mid + market.Ticks(r.Int63n(2*int64(spread)+1)) - spread
	high := open + market.Ticks(r.Int63n(int64(spread)+1))
	low := open - market.Ticks(r.Int63n(int64(spread)+1))
	close := low + market.Ticks(r.Int63n(int64(high-low)+1))

	pos := portfolio.Position{Instrument: mnq, NetQty: 2, CostBasisCts: 2_000_000}
	levels := portfolio.ProtectiveLevels{Stop: mid - spread/2, Target: mid + spread/2}
	if r.Int63n(2) == 1 {
		pos.NetQty = -2
		pos.CostBasisCts = -2_000_000
		levels = portfolio.ProtectiveLevels{Stop: mid + spread/2, Target: mid - spread/2}
	}
	if levels.Stop == levels.Target {
		levels = portfolio.ProtectiveLevels{}
	}

	return market.Bar{
		Instrument: mnq,
		StartTime:  market.LogicalTime(r.Int63n(1_000_000)),
		EndTime:    market.LogicalTime(r.Int63n(1_000_000)) + 1_000_001,
		Open:       open,
		High:       high,
		Low:        low,
		Close:      close,
		Sequence:   uint64(r.Int63n(1_000)),
	}, pos, levels
}

// Property: the same ambiguous bar always resolves the same way.
func TestPropertyIntrabarResolutionIsDeterministic(t *testing.T) {
	const runs = 20
	var policy execution.WorstCaseIntrabar

	var baseline []execution.IntrabarResult
	ambiguous := 0
	for run := 0; run < runs; run++ {
		r := rand.New(rand.NewSource(20240823))
		observed := make([]execution.IntrabarResult, 0, 500)
		for i := 0; i < 500; i++ {
			b, pos, lv := randomProtectedBar(r)
			res, err := policy.Resolve(b, pos, lv)
			if err != nil {
				t.Fatalf("run %d iteration %d: legal input rejected: %v", run, i, err)
			}
			if run == 0 && res.Ambiguous {
				ambiguous++
			}
			observed = append(observed, res)
		}
		if run == 0 {
			baseline = observed
			continue
		}
		if !reflect.DeepEqual(observed, baseline) {
			t.Fatalf("run %d diverged from the baseline run", run)
		}
	}
	if ambiguous == 0 {
		t.Fatal("no bar was ever ambiguous: the property proved nothing")
	}
}

// Property: a resolved exit is never better than its level. The stop may fill
// worse on a gap; the target never fills better than the target.
func TestPropertyIntrabarExitIsNeverBetterThanItsLevel(t *testing.T) {
	r := rand.New(rand.NewSource(20240824))
	var policy execution.WorstCaseIntrabar

	resolved := 0
	for i := 0; i < 5000; i++ {
		b, pos, lv := randomProtectedBar(r)
		res, err := policy.Resolve(b, pos, lv)
		if err != nil {
			t.Fatalf("iteration %d: legal input rejected: %v", i, err)
		}
		if res.Outcome == execution.IntrabarNone {
			continue
		}
		resolved++

		switch {
		case res.Outcome == execution.IntrabarStop && pos.IsLong():
			if res.Price > lv.Stop {
				t.Fatalf("iteration %d: long stop resolved at %d, above its level %d", i, res.Price, lv.Stop)
			}
		case res.Outcome == execution.IntrabarStop && pos.IsShort():
			if res.Price < lv.Stop {
				t.Fatalf("iteration %d: short stop resolved at %d, below its level %d", i, res.Price, lv.Stop)
			}
		case res.Outcome == execution.IntrabarTarget:
			if res.Price != lv.Target {
				t.Fatalf("iteration %d: target resolved at %d, not at its level %d", i, res.Price, lv.Target)
			}
		}
	}
	if resolved == 0 {
		t.Fatal("no bar ever resolved: the property proved nothing")
	}
}

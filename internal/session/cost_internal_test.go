package session

import (
	"runtime"
	"testing"

	"praxis/internal/challenge"
	"praxis/internal/market"
)

func costConfig() Config {
	i := market.Instrument{Symbol: "MNQ", CentsPerTick: 50}
	return Config{
		Instrument: i, StartingBalanceCts: 5_000_000, CommissionPerContractCts: 50,
		Rules: challenge.Rules{
			StartingBalanceCts: 5_000_000, MaxDailyLossCts: 1_000_000,
			ProfitTargetCts: 1_000_000, MaxTotalLossCts: 2_000_000,
		},
	}
}

// observedTo drives a scripted session until its journal holds at least n
// events, and returns it with the logical time to carry on from.
func observedTo(t testing.TB, n int) (*Session, market.LogicalTime) {
	t.Helper()
	cfg := costConfig()
	s, err := New(cfg, 1_000, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.OpenTradingSession(2_000, challenge.SessionID("d1")); err != nil {
		t.Fatalf("OpenTradingSession: %v", err)
	}
	at := market.LogicalTime(3_000)
	for s.JournalLen() < n {
		if err := s.Observe(market.Quote{
			Instrument: cfg.Instrument, Time: at,
			Bid: 20_000, Ask: 20_001, BidSize: 10, AskSize: 10,
		}, 1); err != nil {
			t.Fatalf("Observe: %v", err)
		}
		at += 1_000
	}
	return s, at
}

// bytesPerObservation is what one more observation costs a session whose
// journal is already this long.
//
// Bytes and not allocations: a copy of a slice is one allocation whichever
// length it has, so a count cannot see this at all. TotalAlloc is cumulative
// and unaffected by collection, so the figure is the work actually done.
func bytesPerObservation(t testing.TB, existing int) uint64 {
	t.Helper()
	s, at := observedTo(t, existing)
	const runs = 20

	// Room for everything the window will append. The journal's own slice
	// doubling inside the measurement is the amortised cost of the journal
	// growing, not the cost of a command, and at ten thousand events one
	// doubling dwarfs what is being measured.
	roomy := make([]Event, len(s.journal.events), len(s.journal.events)+runs*8)
	copy(roomy, s.journal.events)
	s.journal.events = roomy

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < runs; i++ {
		if err := s.Observe(market.Quote{
			Instrument: s.cfg.Instrument, Time: at,
			Bid: 20_000, Ask: 20_001, BidSize: 10, AskSize: 10,
		}, 1); err != nil {
			t.Fatalf("Observe: %v", err)
		}
		at += 1_000
	}
	runtime.ReadMemStats(&after)
	return (after.TotalAlloc - before.TotalAlloc) / runs
}

// Scenario: a command costs the same whatever the journal already holds
//
//	Given a session whose journal is a hundred events long, and one whose
//	  journal is ten thousand
//	Then one more observation costs the second no more than it costs the first.
//
// A command handed its own events by copying the whole journal made a session
// quadratic in its own length: at ten thousand events an observation allocated
// 164KB to deliver the two or three it produced, against 2.2KB at a hundred. A
// real session is far longer than ten thousand observations.
//
// The measure is bytes and not allocations, because a copy of a slice is one
// allocation whichever length it has: a count is blind to this by construction.
func TestACommandCostsTheSameOnALongJournalAsOnAShortOne(t *testing.T) {
	short := bytesPerObservation(t, 100)
	long := bytesPerObservation(t, 10_000)
	t.Logf("100 events: %d bytes; 10,000 events: %d bytes", short, long)

	// Before the fix this ratio was about seventy-three.
	if long > 2*short {
		t.Fatalf("a command costs %d bytes on a long journal and %d on a short one, "+
			"so it is paying for the journal's length rather than its own events",
			long, short)
	}
}

// BenchmarkObservationOnALongJournal is the figure itself, for anyone who wants
// it rather than the property.
func BenchmarkObservationOnALongJournal(b *testing.B) {
	s, at := observedTo(b, 10_000)
	q := market.Quote{
		Instrument: s.cfg.Instrument, Bid: 20_000, Ask: 20_001, BidSize: 10, AskSize: 10,
	}
	b.ReportAllocs()
	for b.Loop() {
		at += 1_000
		q.Time = at
		if err := s.Observe(q, 1); err != nil {
			b.Fatalf("Observe: %v", err)
		}
	}
}

// Scenario: what a command is handed is its own
//
//	Given the events a command produced
//	When the caller keeps them and appends to what it kept
//	Then the journal's next append does not write into them.
//
// Returning j.events[n:] would be cheaper than copying and wrong: the slice
// would carry the journal's spare capacity with it, so a committer that kept
// the batch and grew it would be writing into the slot the next event is about
// to take — and the journal would then write over what the committer put
// there. persistence.Writer keeps exactly this slice on its Batch.
func TestWhatACommandIsHandedIsItsOwn(t *testing.T) {
	j := &Journal{}
	ended := func(seq uint64) Event {
		return SessionEnded{
			Envelope:  Envelope{Time: market.LogicalTime(seq * 1_000), Sequence: seq, Kind: KindSessionEnded},
			SessionID: challenge.SessionID("d1"),
		}
	}
	for seq := uint64(1); seq <= 3; seq++ {
		if err := j.Append(ended(seq)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	kept := j.EventsSince(1)
	marker := ended(99)
	kept = append(kept, marker)

	if err := j.Append(ended(4)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if kept[len(kept)-1] != marker {
		t.Fatal("the journal wrote into a batch the caller was still holding")
	}
	if j.Len() != 4 {
		t.Fatalf("journal: got %d events, want 4", j.Len())
	}
}

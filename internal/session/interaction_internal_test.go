package session

import (
	"errors"
	"testing"

	"praxis/internal/challenge"
	"praxis/internal/market"
)

// Scenario: the journal asks the chronology about every event, not only heads
//
//	Given a session that has recorded a command
//	When something the log itself required is built carrying a person's clock
//	Then record refuses it.
//
// The door checks the head of a command, where a refusal costs nothing. This is
// the other half: a derived event cannot be reached from outside, so nothing a
// caller does can produce one — which is exactly why the check has to live
// where every event passes rather than where the commands arrive. Without it,
// the writing side would be running a smaller rule than its readers, one event
// kind at a time, which is the asymmetry this whole machine exists to close.
func TestRecordRefusesAStampOnSomethingDerived(t *testing.T) {
	instrument := market.Instrument{Symbol: "MNQ", CentsPerTick: 50}
	cfg := Config{
		Instrument: instrument, SubjectID: "t-01", Pacing: PacingPilot,
		StartingBalanceCts: 5_000_000, CommissionPerContractCts: 50,
		Rules: challenge.Rules{
			StartingBalanceCts: 5_000_000, MaxDailyLossCts: 100_000,
			ProfitTargetCts: 100_000, MaxTotalLossCts: 200_000,
		},
	}
	s, err := New(cfg, 1_000, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	before := s.JournalLen()

	// A leg cancelled by its sibling: nobody commanded it, so nobody timed it.
	err = s.record(2_000, KindOrderCancelled, func(e Envelope) Event {
		return OrderCancelled{
			Envelope: e, OrderID: "praxis:1:stop", RemainingQty: 1,
			Reason:  CancelledByOCO,
			Decided: Decision{GestureID: "g-1", AtUTCNanos: 1, Segment: 1, ElapsedNanos: 1},
		}
	})
	if !errors.Is(err, ErrInteractionStamp) {
		t.Fatalf("record: got %v, want %v", err, ErrInteractionStamp)
	}
	if s.JournalLen() != before {
		t.Fatalf("journal: got %d events, want the %d it had", s.JournalLen(), before)
	}

	// And the same event without the stamp is recorded, so the refusal is
	// about the clock and not about the event.
	err = s.record(2_000, KindOrderCancelled, func(e Envelope) Event {
		return OrderCancelled{
			Envelope: e, OrderID: "praxis:1:stop", RemainingQty: 1,
			Reason: CancelledByOCO,
		}
	})
	if err != nil {
		t.Fatalf("record without a stamp: %v", err)
	}
	if s.JournalLen() != before+1 {
		t.Fatalf("journal: got %d events, want %d", s.JournalLen(), before+1)
	}
}

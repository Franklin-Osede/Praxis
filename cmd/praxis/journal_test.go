package main

import (
	"os"
	"testing"

	"praxis/internal/adapters/persistence"
	"praxis/internal/challenge"
	"praxis/internal/market"
	"praxis/internal/session"
)

var mnq = market.Instrument{Symbol: "MNQ", CentsPerTick: 50}

func writeJournalForCLI(t *testing.T, path string) {
	t.Helper()
	w, err := persistence.OpenWriter(path, persistence.DurableEveryBatch)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	defer w.Close()

	cfg := session.Config{
		Instrument:               mnq,
		StartingBalanceCts:       5_000_000,
		CommissionPerContractCts: 50,
		Rules:                    challenge.Rules{StartingBalanceCts: 5_000_000, MaxDailyLossCts: 100_000},
	}
	s, err := session.New(cfg, 1_000, w)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.OpenTradingSession(2_000, "d1"); err != nil {
		t.Fatalf("OpenTradingSession: %v", err)
	}
	if err := s.Observe(market.Quote{
		Instrument: mnq, Time: 3_000, Bid: 20_000, Ask: 20_001, BidSize: 10, AskSize: 10,
	}); err != nil {
		t.Fatalf("Observe: %v", err)
	}
}

func truncateJournalForCLI(t *testing.T, path string, drop int) {
	t.Helper()
	whole, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if err := os.WriteFile(path, whole[:len(whole)-drop], 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

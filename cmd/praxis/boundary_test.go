package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"praxis/internal/adapters/marketdata"
	"praxis/internal/adapters/persistence"
	"praxis/internal/challenge"
	"praxis/internal/session"
)

// Scenario: a run interrupted exactly on a boundary is resumable
//
//	Given a journal whose last confirmed batch closed a trading session — what a
//	  crash between the two boundary commands leaves, and what an applied repair
//	  produces when the interrupted batch was the boundary
//	When praxis replay is run against the file it came from
//	Then it resumes, crosses into the next session, and produces byte for byte
//	  the journal an uninterrupted run would have.
//
// Before this it exited 2 — documented as "exists but is not a journal, or is
// damaged beyond truncation", neither of which was true — while store inspect
// called the file clean and repair had nothing to do. Permanently unrunnable,
// with every diagnostic reporting health.
func TestAJournalEndingOnABoundaryResumes(t *testing.T) {
	binary := buildPraxis(t)
	dir := t.TempDir()
	market := writeMarket(t, dir, "market.csv", marketFile)

	whole := filepath.Join(dir, "whole.praxis")
	if _, code := runPraxis(t, binary, replayArgs(market, whole)...); code != exitClean {
		t.Fatal("the uninterrupted run failed")
	}
	want, err := os.ReadFile(whole)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// A journal that stops on the boundary. The CLI cannot make one — the end
	// of a file is not a boundary, so it always leaves the last session open —
	// so it is built here, through the same writer, and every batch in it is
	// confirmed.
	journal := filepath.Join(dir, "journal.praxis")
	buildToBoundary(t, market, journal)

	if out, code := runPraxis(t, binary, "store", "verify", journal); code != exitClean {
		t.Fatalf("the journal is not provable before we start: exit %d\n%s", code, out)
	}

	out, code := runPraxis(t, binary, "replay", market, "--journal", journal)
	if code != exitClean {
		t.Fatalf("resuming on a boundary: exit %d\n%s", code, out)
	}
	// It crossed the boundary rather than tripping over it: the rows of the
	// second session were consumed, and it is that session — not the one that
	// had already closed — the run leaves open at the end of the file.
	if !strings.Contains(out, "consumed:  2 new rows") {
		t.Fatalf("the resumed run did not cross the boundary:\n%s", out)
	}
	if !strings.Contains(out, "trading session d2 is still open") {
		t.Fatalf("the run does not end in the session the file ends in:\n%s", out)
	}

	got, err := os.ReadFile(journal)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(want) {
		t.Fatal("a run resumed on a boundary produced a different journal from an uninterrupted one")
	}
}

// buildToBoundary writes a journal that consumes the first trading session and
// then closes it, with nothing after.
func buildToBoundary(t *testing.T, marketPath, journalPath string) {
	t.Helper()
	file, err := os.Open(marketPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer file.Close()
	feed, err := marketdata.Read(file)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	writer, err := persistence.OpenWriter(journalPath, persistence.DurableEveryBatch)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	defer writer.Close()

	cfg := session.Config{
		Instrument:         feed.Instrument,
		StartingBalanceCts: 5_000_000, CommissionPerContractCts: 50,
		Rules: challenge.Rules{
			StartingBalanceCts: 5_000_000, MaxDailyLossCts: 100_000,
			ProfitTargetCts: 300_000,
		},
	}
	s, err := session.New(cfg, feed.Observations[0].Quote.Time, writer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Every row of the first trading session, and then its boundary.
	rows := 0
	first := feed.Observations[0].SessionID
	for _, o := range feed.Observations {
		if o.SessionID != first {
			break
		}
		rows++
	}
	if err := marketdata.Drive(s, &marketdata.Feed{
		Instrument: feed.Instrument, Observations: feed.Observations[:rows],
	}, 0); err != nil {
		t.Fatalf("Drive: %v", err)
	}
	if err := s.EndTradingSession(feed.Observations[rows].Quote.Time); err != nil {
		t.Fatalf("EndTradingSession: %v", err)
	}
}

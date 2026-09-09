package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"praxis/internal/adapters/marketdata"
	"praxis/internal/adapters/persistence"
	"praxis/internal/challenge"
	"praxis/internal/market"
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

// Scenario: a run whose evaluation ended is clean, and running it again is free
//
//	Given a journal whose evaluation has ended with market left in the file
//	When praxis replay is run against that file, twice
//	Then both runs exit clean, both report the outcome and what was left, and
//	  the journal after the second is byte for byte what it was after the first.
//
// This is the test that matters, because it is the only one that would have
// caught the run stopping in the wrong place. Reacting to the kernel's refusal
// instead of checking before the boundary still stops the run — but it commits
// a SessionEnded on the way out, so the journal grows every time it is run.
func TestARunPastATerminalEvaluationIsCleanAndIdempotent(t *testing.T) {
	binary := buildPraxis(t)
	dir := t.TempDir()
	market := writeMarket(t, dir, "market.csv", marketFileWithACrash)
	journal := filepath.Join(dir, "journal.praxis")
	buildToFailure(t, market, journal)

	out, code := runPraxis(t, binary, "replay", market, "--journal", journal)
	if code != exitClean {
		t.Fatalf("a run whose evaluation ended is not a failure: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "challenge: failed") {
		t.Fatalf("the outcome is not reported:\n%s", out)
	}
	if !strings.Contains(out, "remaining: 2 market rows not consumed") {
		t.Fatalf("what was left is not reported:\n%s", out)
	}
	// The rows are counted from what the journal holds, not from what the file
	// offers: this run took none, because the evaluation had already ended.
	if !strings.Contains(out, "verified:  2 rows") || !strings.Contains(out, "consumed:  0 new rows") {
		t.Fatalf("the rows are counted from the file rather than the journal:\n%s", out)
	}
	after, err := os.ReadFile(journal)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	out, code = runPraxis(t, binary, "replay", market, "--journal", journal)
	if code != exitClean {
		t.Fatalf("the second run: exit %d\n%s", code, out)
	}
	again, err := os.ReadFile(journal)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(again) != string(after) {
		t.Fatalf("running it again changed the journal by %d bytes", len(again)-len(after))
	}
}

const marketFileWithACrash = `praxis.market.v1,MNQ,50
time,sequence,session_id,bid,ask,bid_size,ask_size
3000,1,d1,20000,20001,50,50
4000,1,d1,19000,19001,50,50
5000,1,d2,19990,19991,50,50
6000,1,d2,19985,19986,50,50
`

// buildToFailure writes a journal whose evaluation ends inside the first
// trading session. The CLI cannot make one: it submits no orders.
func buildToFailure(t *testing.T, marketPath, journalPath string) {
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
	if err := marketdata.Drive(s, &marketdata.Feed{
		Instrument: feed.Instrument, Observations: feed.Observations[:1],
	}, 0); err != nil {
		t.Fatalf("Drive: %v", err)
	}
	buy, err := market.NewMarketOrder("o-1", feed.Instrument, market.SideBuy, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitOrder(buy, session.Decision{}); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
	if err := marketdata.Drive(s, feed, 1); err != nil {
		t.Fatalf("Drive: %v", err)
	}
	if !s.ChallengeEnded() {
		t.Fatal("the fixture did not end its evaluation")
	}
}

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const marketFile = `praxis.market.v1,MNQ,50
time,sequence,session_id,bid,ask,bid_size,ask_size
3000,1,d1,20000,20001,10,10
3000,2,d1,20001,20002,8,12
4000,1,d1,20010,20011,10,10
5000,1,d2,19990,19991,10,10
6000,1,d2,19985,19986,10,10
`

func writeMarket(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func replayArgs(market, journal string) []string {
	return []string{"replay", market, "--journal", journal,
		"--starting-balance", "5000000", "--commission", "50",
		"--max-daily-loss", "100000", "--profit-target", "300000"}
}

// Scenario: a market file drives a session into a durable journal
func TestReplayARunFromNothing(t *testing.T) {
	binary := buildPraxis(t)
	dir := t.TempDir()
	market := writeMarket(t, dir, "market.csv", marketFile)
	journal := filepath.Join(dir, "journal.praxis")

	out, code := runPraxis(t, binary, replayArgs(market, journal)...)
	if code != exitClean {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, want := range []string{
		"verified:  0 rows", "consumed:  5 new rows", "challenge: active",
		"open:      trading session d2 is still open",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output does not report %q:\n%s", want, out)
		}
	}

	if _, code := runPraxis(t, binary, "store", "inspect", journal); code != exitClean {
		t.Fatalf("the journal it wrote is not clean: exit %d", code)
	}
}

// Scenario: running the same file twice adds nothing
//
// The second run verifies every row it already holds and finds none left to
// consume, so the journal is not touched at all.
func TestReplayTwiceChangesNothing(t *testing.T) {
	binary := buildPraxis(t)
	dir := t.TempDir()
	market := writeMarket(t, dir, "market.csv", marketFile)
	journal := filepath.Join(dir, "journal.praxis")

	if _, code := runPraxis(t, binary, replayArgs(market, journal)...); code != exitClean {
		t.Fatal("the first run failed")
	}
	first, err := os.ReadFile(journal)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	out, code := runPraxis(t, binary, "replay", market, "--journal", journal)
	if code != exitClean {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "verified:  5 rows") || !strings.Contains(out, "consumed:  0 new rows") {
		t.Fatalf("the second run did not verify the whole file:\n%s", out)
	}

	second, err := os.ReadFile(journal)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(second) != string(first) {
		t.Fatal("running the same file twice changed the journal")
	}
}

// Scenario: a run interrupted, repaired and continued produces exactly the
// journal an uninterrupted run would have
func TestAnInterruptedRunResumesToTheSameJournal(t *testing.T) {
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

	// A run cut off part way: the first three rows only.
	partialMarket := writeMarket(t, dir, "partial.csv", strings.Join(strings.SplitN(marketFile, "\n", 6)[:5], "\n")+"\n")
	journal := filepath.Join(dir, "journal.praxis")
	if _, code := runPraxis(t, binary, replayArgs(partialMarket, journal)...); code != exitClean {
		t.Fatal("the partial run failed")
	}

	// Then torn mid-append, as a process killed between batches would leave it.
	torn, err := os.ReadFile(journal)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if err := os.WriteFile(journal, append(torn, []byte("BATCH 000000")...), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	out, code := runPraxis(t, binary, "replay", market, "--journal", journal)
	if code != exitRepairable || !strings.Contains(out, "praxis store inspect") {
		t.Fatalf("a damaged tail was not sent to repair: exit %d\n%s", code, out)
	}

	if _, code := runPraxis(t, binary, "store", "repair", journal, "--apply"); code != exitClean {
		t.Fatal("the repair failed")
	}

	out, code = runPraxis(t, binary, "replay", market, "--journal", journal)
	if code != exitClean {
		t.Fatalf("resuming: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "verified:  3 rows") || !strings.Contains(out, "consumed:  2 new rows") {
		t.Fatalf("the resumed run did not continue where it stopped:\n%s", out)
	}

	got, err := os.ReadFile(journal)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(want) {
		t.Fatal("a resumed run produced a different journal from an uninterrupted one")
	}
}

// Scenario: a file that changed is refused before anything is appended
func TestReplayRefusesAChangedMarketFile(t *testing.T) {
	binary := buildPraxis(t)
	dir := t.TempDir()
	market := writeMarket(t, dir, "market.csv", marketFile)
	journal := filepath.Join(dir, "journal.praxis")

	if _, code := runPraxis(t, binary, replayArgs(market, journal)...); code != exitClean {
		t.Fatal("the first run failed")
	}
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// The same number of rows, one of them altered.
	changed := strings.Replace(marketFile, "4000,1,d1,20010,20011,10,10", "4000,1,d1,20099,20100,10,10", 1)
	writeMarket(t, dir, "market.csv", changed)

	out, code := runPraxis(t, binary, "replay", market, "--journal", journal)
	if code != exitFatal {
		t.Fatalf("a changed file was accepted: exit %d\n%s", code, out)
	}
	after, err := os.ReadFile(journal)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("a rejected run appended to the journal")
	}
}

// Scenario: a row appended to the file is picked up
func TestReplayContinuesIntoAGrownFile(t *testing.T) {
	binary := buildPraxis(t)
	dir := t.TempDir()
	market := writeMarket(t, dir, "market.csv", marketFile)
	journal := filepath.Join(dir, "journal.praxis")

	if _, code := runPraxis(t, binary, replayArgs(market, journal)...); code != exitClean {
		t.Fatal("the first run failed")
	}

	writeMarket(t, dir, "market.csv", marketFile+"7000,1,d2,19980,19981,10,10\n")

	out, code := runPraxis(t, binary, "replay", market, "--journal", journal)
	if code != exitClean {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "verified:  5 rows") || !strings.Contains(out, "consumed:  1 new rows") {
		t.Fatalf("the grown file was not continued:\n%s", out)
	}
}

// Scenario: a journal belonging to another instrument is refused
func TestReplayRefusesAnotherInstrumentsJournal(t *testing.T) {
	binary := buildPraxis(t)
	dir := t.TempDir()
	market := writeMarket(t, dir, "market.csv", marketFile)
	journal := filepath.Join(dir, "journal.praxis")

	if _, code := runPraxis(t, binary, replayArgs(market, journal)...); code != exitClean {
		t.Fatal("the first run failed")
	}

	other := writeMarket(t, dir, "mes.csv", strings.Replace(marketFile, "MNQ,50", "MES,125", 1))
	out, code := runPraxis(t, binary, "replay", other, "--journal", journal)
	if code != exitFatal || !strings.Contains(out, "MES") {
		t.Fatalf("another instrument was accepted: exit %d\n%s", code, out)
	}
}

// Scenario: a new journal will not start without its configuration
func TestANewJournalNeedsItsConfiguration(t *testing.T) {
	binary := buildPraxis(t)
	dir := t.TempDir()
	market := writeMarket(t, dir, "market.csv", marketFile)

	out, code := runPraxis(t, binary, "replay", market, "--journal", filepath.Join(dir, "journal.praxis"))
	if code != exitFatal || !strings.Contains(out, "starting-balance") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

// Scenario: the flags cannot reconfigure a journal that already has a history
func TestFlagsCannotReconfigureAnExistingJournal(t *testing.T) {
	binary := buildPraxis(t)
	dir := t.TempDir()
	market := writeMarket(t, dir, "market.csv", marketFile)
	journal := filepath.Join(dir, "journal.praxis")

	if _, code := runPraxis(t, binary, replayArgs(market, journal)...); code != exitClean {
		t.Fatal("the first run failed")
	}
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if _, code := runPraxis(t, binary, "replay", market, "--journal", journal,
		"--starting-balance", "999999999", "--max-daily-loss", "1"); code != exitClean {
		t.Fatal("resuming with contradictory flags failed for the wrong reason")
	}

	after, err := os.ReadFile(journal)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("the flags changed a journal that already had a history")
	}
}

// Scenario: a journal another process holds is busy, not broken
func TestReplayOnABusyJournal(t *testing.T) {
	binary := buildPraxis(t)
	dir := t.TempDir()
	market := writeMarket(t, dir, "market.csv", marketFile)
	journal := filepath.Join(dir, "journal.praxis")
	writeJournalForCLI(t, journal)

	held := holdJournal(t, journal)
	defer held()

	_, code := runPraxis(t, binary, "replay", market, "--journal", journal)
	if code != exitBusy {
		t.Fatalf("exit: got %d, want %d", code, exitBusy)
	}
}

// Scenario: a journal that holds only its configuration still knows what
// instrument it is for
//
// Comparing observations cannot catch this: there are none. A run interrupted
// straight after its configuration was committed would otherwise resume
// against any instrument at all.
func TestAJournalWithOnlyItsConfigurationChecksTheInstrument(t *testing.T) {
	binary := buildPraxis(t)
	dir := t.TempDir()
	market := writeMarket(t, dir, "market.csv", marketFile)
	journal := filepath.Join(dir, "journal.praxis")

	if _, code := runPraxis(t, binary, replayArgs(market, journal)...); code != exitClean {
		t.Fatal("the first run failed")
	}
	truncateToFirstBatch(t, journal)

	if _, code := runPraxis(t, binary, "store", "inspect", journal); code != exitClean {
		t.Fatal("the truncated journal is not clean")
	}

	other := writeMarket(t, dir, "mes.csv", strings.Replace(marketFile, "MNQ,50", "MES,125", 1))
	out, code := runPraxis(t, binary, "replay", other, "--journal", journal)
	if code != exitFatal {
		t.Fatalf("another instrument was accepted: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "MES") || !strings.Contains(out, "MNQ") {
		t.Fatalf("the refusal does not name both instruments:\n%s", out)
	}
}

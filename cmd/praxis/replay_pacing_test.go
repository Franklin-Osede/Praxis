package main

import (
	"os"
	"strings"
	"testing"
)

// Scenario: replay refuses a journal somebody traded, and writes nothing
//
//	Given a journal produced through the interface, at pilot pacing
//	When praxis replay is pointed at it
//	Then it refuses, names praxis ui as the command to use, and the journal is
//	  byte for byte what it was.
//
// The scripted feed advances the market with nobody watching. Every row it
// added would be one no participant was shown, and the presentations the
// journal holds would stop matching its observations — a hole in the one
// quantity the pilots record, in a journal that still verifies clean.
func TestReplayRefusesAJournalSomebodyTraded(t *testing.T) {
	binary := buildPraxis(t)
	dir := t.TempDir()
	marketPath := writeMarket(t, dir, "market.csv", marketFile)
	journalPath := pilotJournal(t, dir, marketPath)

	before, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	out, code := runPraxis(t, binary, "replay", marketPath, "--journal", journalPath)
	if code != exitFatal {
		t.Fatalf("exit %d, want %d\n%s", code, exitFatal, out)
	}
	for _, want := range []string{"praxis ui", "pilot"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the refusal does not say %q:\n%s", want, out)
		}
	}

	after, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("the refused command changed the journal")
	}
}

// Scenario: a scripted journal is still replayed
//
// The guard is about who produced the journal, not about resuming.
func TestReplayStillResumesAScriptedJournal(t *testing.T) {
	binary := buildPraxis(t)
	dir := t.TempDir()
	marketPath := writeMarket(t, dir, "market.csv", marketFile)
	journalPath := dir + "/scripted.praxis"

	if out, code := runPraxis(t, binary, replayArgs(marketPath, journalPath)...); code != exitClean {
		t.Fatalf("the first run: exit %d\n%s", code, out)
	}
	out, code := runPraxis(t, binary, "replay", marketPath, "--journal", journalPath)
	if code != exitClean {
		t.Fatalf("resuming a scripted journal: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "verified:  5 rows") {
		t.Fatalf("the resumed run did not verify the file:\n%s", out)
	}
}

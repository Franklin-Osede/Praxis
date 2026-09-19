package main

import (
	"path/filepath"
	"strings"
	"testing"

	"praxis/internal/adapters/ui"
	"praxis/internal/challenge"
	"praxis/internal/session"
)

// pilotJournal writes a journal somebody traded and closes it, leaving a run
// for the command to resume.
func pilotJournal(t *testing.T, dir, marketPath string) string {
	t.Helper()
	journalPath := filepath.Join(dir, "pilot.praxis")
	server, err := ui.Open(ui.Options{
		Market: marketPath, Journal: journalPath,
		New: session.Config{
			SubjectID: "t-01", RunID: "r-01", Pacing: session.PacingPilot,
			StartingBalanceCts: 5_000_000, CommissionPerContractCts: 50,
			Rules: challenge.Rules{
				StartingBalanceCts: 5_000_000,
				MaxDailyLossCts:    100_000,
				ProfitTargetCts:    300_000,
			},
		},
	})
	if err != nil {
		t.Fatalf("ui.Open: %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return journalPath
}

// Scenario: the command refuses to continue somebody else's run under a new
// label
//
// The operator who points the next participant at the journal still open on
// their desk types the new subject, and every decision that follows would be
// recorded under the last participant's label. The journal keeps its own
// configuration, so the disagreement is the operator's to resolve.
func TestUIRefusesLabelsTheJournalDoesNotHold(t *testing.T) {
	binary := buildPraxis(t)
	dir := t.TempDir()
	marketPath := writeMarket(t, dir, "market.csv", marketFile)
	journalPath := pilotJournal(t, dir, marketPath)

	for _, given := range [][]string{
		{"--subject", "t-99"},
		{"--run-id", "r-99"},
		{"--subject", "t-99", "--run-id", "r-99"},
	} {
		args := append([]string{"ui", marketPath, "--journal", journalPath}, given...)
		out, code := runPraxis(t, binary, args...)
		if code != exitFatal {
			t.Fatalf("%v: exit %d, want %d\n%s", given, code, exitFatal, out)
		}
		if !strings.Contains(out, "names a different run") {
			t.Fatalf("%v: the refusal does not name the fault:\n%s", given, out)
		}
	}
}

// Scenario: resuming with no labels, or with the journal's own, is allowed
//
// It cannot be run to completion here — the command serves until it is stopped
// — so this checks the refusal is not raised before the server opens.
func TestUIAcceptsTheLabelsTheJournalHolds(t *testing.T) {
	dir := t.TempDir()
	marketPath := writeMarket(t, dir, "market.csv", marketFile)
	journalPath := pilotJournal(t, dir, marketPath)

	for _, declared := range []ui.Options{
		{Market: marketPath, Journal: journalPath},
		{Market: marketPath, Journal: journalPath, DeclaredSubject: "t-01", DeclaredRunID: "r-01"},
	} {
		server, err := ui.Open(declared)
		if err != nil {
			t.Fatalf("Open %+v: %v", declared, err)
		}
		if err := server.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}

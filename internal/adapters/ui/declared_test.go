package ui_test

import (
	"errors"
	"strings"
	"testing"

	"praxis/internal/adapters/ui"
)

// journalOf writes a pilot journal and closes it, so the next Open resumes.
func journalOf(t *testing.T) (marketPath, journalPath string) {
	t.Helper()
	marketPath, journalPath = paths(t)
	s := open(t, marketPath, journalPath, pilotConfig())
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return marketPath, journalPath
}

// Scenario: a resumed journal refuses a subject that is not its own
//
//	Given a journal traded by t-01 in run r-01
//	When it is resumed with --subject t-99
//	Then the server refuses before the session opens, naming both labels.
//
// A journal's configuration comes from its own SessionStarted and never from a
// flag, so a flag that disagrees is the operator saying something untrue about
// the run. Ignoring it silently is how the next participant's decisions get
// recorded under the last one's label, with only the subject line of a verify
// to show it.
func TestAResumedJournalRefusesADifferentSubject(t *testing.T) {
	marketPath, journalPath := journalOf(t)

	_, err := ui.Open(ui.Options{
		Market: marketPath, Journal: journalPath, DeclaredSubject: "t-99",
	})
	if !errors.Is(err, ui.ErrDeclaredMismatch) {
		t.Fatalf("Open: %v, want a declared-configuration mismatch", err)
	}
	for _, want := range []string{"t-01", "t-99", "subject"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not say %q: %v", want, err)
		}
	}
}

// Scenario: a resumed journal refuses a run identity that is not its own
//
// It matters more than the subject: an anchor names the run it certifies, so a
// journal continued under another label is one no anchor can be told from
// another's.
func TestAResumedJournalRefusesADifferentRunIdentity(t *testing.T) {
	marketPath, journalPath := journalOf(t)

	_, err := ui.Open(ui.Options{
		Market: marketPath, Journal: journalPath, DeclaredRunID: "r-99",
	})
	if !errors.Is(err, ui.ErrDeclaredMismatch) {
		t.Fatalf("Open: %v, want a declared-configuration mismatch", err)
	}
	for _, want := range []string{"r-01", "r-99", "run"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not say %q: %v", want, err)
		}
	}
}

// Scenario: the labels the journal already holds are accepted, and so is
// declaring nothing
//
// An absent flag is not an empty subject: the operator who types the same
// labels again and the operator who types none are both continuing the run
// that is there.
func TestAResumedJournalAcceptsItsOwnLabelsAndSilence(t *testing.T) {
	marketPath, journalPath := journalOf(t)

	for _, declared := range []ui.Options{
		{Market: marketPath, Journal: journalPath},
		{Market: marketPath, Journal: journalPath, DeclaredSubject: "t-01", DeclaredRunID: "r-01"},
	} {
		s, err := ui.Open(declared)
		if err != nil {
			t.Fatalf("Open %+v: %v", declared, err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}

package persistence_test

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"testing"

	"praxis/internal/adapters/persistence"
	"praxis/internal/market"
	"praxis/internal/session"
)

// These tests hold the distinction ADR-015 exists to keep visible: a journal
// proving its own coherence is not a journal agreeing with an anchor. Every one
// of them starts from a journal the real writer produced, because a truncation
// that only a hand-built fixture exhibits would prove nothing about the format.

// commit writes one batch and releases the lock, so the next commit can take
// it. The size afterwards is the exact boundary a participant would cut at, and
// taking it from the file rather than computing it means the test cuts where
// the format actually ends a batch.
func commit(t *testing.T, path string, events []session.Event) int64 {
	t.Helper()
	w, err := persistence.OpenWriter(path, persistence.DurableEveryBatch)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	if _, err := w.Append(events); err != nil {
		w.Close()
		t.Fatalf("Append: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	return info.Size()
}

// split cuts the fixture after its first observation, so that either batch can
// be altered in a way nothing downstream recomputes.
func split(t *testing.T) (head, tail []session.Event) {
	t.Helper()
	events := realSessionEvents(t)
	for n, e := range events {
		if _, ok := e.(session.MarketObserved); ok {
			head, tail = events[:n+1], events[n+1:]
			if len(tail) == 0 {
				t.Fatal("the fixture ends at its first observation")
			}
			return head, tail
		}
	}
	t.Fatal("the fixture carries no observation")
	return nil, nil
}

// twoBatches writes a journal in two commits, returning the path and the size
// the file had after the first.
func twoBatches(t *testing.T) (path string, afterFirst int64) {
	t.Helper()
	head, tail := split(t)
	path = journalPath(t)
	afterFirst = commit(t, path, head)
	commit(t, path, tail)
	return path, afterFirst
}

func anchorOf(t *testing.T, path string) persistence.Anchor {
	t.Helper()
	a, err := persistence.AnchorOf(path)
	if err != nil {
		t.Fatalf("AnchorOf: %v", err)
	}
	return a
}

// altered changes one displayed size on the first observation it finds. It is
// deliberately a field nothing downstream recomputes, so the forgery survives
// everything the file can check and only the digest refuses it.
func altered(t *testing.T, events []session.Event) []session.Event {
	t.Helper()
	out := append([]session.Event{}, events...)
	for n, e := range out {
		if observed, ok := e.(session.MarketObserved); ok {
			observed.Quote.BidSize += market.Qty(1)
			out[n] = observed
			return out
		}
	}
	t.Fatal("these events carry no observation to alter")
	return nil
}

// Scenario: a journal and the anchor taken from it agree
func TestAnchorMatchesTheJournalItCameFrom(t *testing.T) {
	path, _ := twoBatches(t)
	anchor := anchorOf(t, path)

	if anchor.LastBatch != 2 {
		t.Fatalf("anchor names batch %d, want 2", anchor.LastBatch)
	}
	if err := anchor.Validate(); err != nil {
		t.Fatalf("the anchor a journal produced does not validate: %v", err)
	}
	if err := persistence.CheckAnchor(path, anchor); err != nil {
		t.Fatalf("CheckAnchor: %v", err)
	}
}

// Scenario: the last batch is removed at its exact boundary
//
// This is the reported defect, as a test. The file that remains is a valid
// journal — that is the whole difficulty — so nothing inside it can report the
// removal, and the anchor is what does.
func TestAnchorDetectsARemovedFinalBatch(t *testing.T) {
	path, afterFirst := twoBatches(t)
	anchor := anchorOf(t, path)

	if err := os.Truncate(path, afterFirst); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	if err := persistence.CheckAnchor(path, anchor); !errors.Is(err, persistence.ErrAnchorUnreached) {
		t.Fatalf("CheckAnchor after truncation: got %v, want ErrAnchorUnreached", err)
	}
}

// Scenario: the truncated journal is still coherent
//
// The two claims are separate and this is the test that says so. If it ever
// fails because the truncated file stopped reading or stopped replaying, the
// anchor has been justified by the wrong argument and ADR-015's reasoning needs
// revisiting rather than the code.
func TestATruncatedJournalIsStillCoherent(t *testing.T) {
	path, afterFirst := twoBatches(t)
	if err := os.Truncate(path, afterFirst); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	j := readFile(t, path)
	if j.Tail != persistence.TailComplete {
		t.Fatalf("tail is %v, want complete: a cut at a batch boundary is not damage", j.Tail)
	}
	if len(j.Batches) != 1 {
		t.Fatalf("%d batches survived, want 1", len(j.Batches))
	}
	if _, err := session.Replay(j.Events()); err != nil {
		t.Fatalf("Replay of the truncated prefix: %v", err)
	}
}

// Scenario: an anchor taken earlier still holds over a journal that grew
//
// A checkpoint anchors a prefix and the run continues past it. The check has to
// compare the bytes the anchor covers and not the whole file, or the next
// legitimate command would invalidate every anchor taken before it.
func TestAnEarlierAnchorHoldsOverAJournalThatGrew(t *testing.T) {
	head, tail := split(t)
	path := journalPath(t)

	commit(t, path, head)
	checkpoint := anchorOf(t, path)
	commit(t, path, tail)

	if err := persistence.CheckAnchor(path, checkpoint); err != nil {
		t.Fatalf("CheckAnchor against a grown journal: %v", err)
	}
}

// Scenario: an unconfirmed tail is not part of the anchor
//
// AnchorOf says so in a comment, and today it is true by construction. This is
// what fails if the arithmetic behind EndOffset or ConfirmedBytes is ever
// changed, which is the only way it would stop being true — quietly.
func TestAnUnconfirmedTailDoesNotMoveTheAnchor(t *testing.T) {
	path, _ := twoBatches(t)
	clean := anchorOf(t, path)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	// A batch header cut short: the ordinary shape of a process killed
	// mid-append, and the one the reader calls incomplete rather than damaged.
	if _, err := f.WriteString("BATCH 0000000000000000"); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if j := readFile(t, path); j.Tail != persistence.TailIncomplete {
		t.Fatalf("tail is %v, want incomplete", j.Tail)
	}

	if got := anchorOf(t, path); got != clean {
		t.Fatalf("an unconfirmed tail moved the anchor:\n %+v\n %+v", got, clean)
	}
	if err := persistence.CheckAnchor(path, clean); err != nil {
		t.Fatalf("CheckAnchor over an unconfirmed tail: %v", err)
	}
}

// Scenario: the anchored batch is rewritten and its checksum recomputed
//
// CRC32C is a checksum and not a signature, so a participant can alter a
// payload and reframe it so the checksum matches again. Nothing in the file
// refuses that; the anchor's digest is what does.
func TestAnchorDetectsARewrittenBatchWithAValidChecksum(t *testing.T) {
	path, afterFirst := twoBatches(t)
	anchor := anchorOf(t, path)
	_, tail := split(t)

	forged, err := persistence.EncodeBatch(2, altered(t, tail), persistence.EventVersion)
	if err != nil {
		t.Fatalf("EncodeBatch: %v", err)
	}
	kept, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	rebuilt := append(append([]byte{}, kept[:afterFirst]...), forged...)
	if err := os.WriteFile(path, rebuilt, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// The forgery is well formed: it reads back without complaint, which is the
	// premise of the test rather than an aside.
	if j := readFile(t, path); len(j.Batches) != 2 || j.Tail != persistence.TailComplete {
		t.Fatalf("the forged journal did not read back whole: %d batches, tail %v", len(j.Batches), j.Tail)
	}
	if err := persistence.CheckAnchor(path, anchor); !errors.Is(err, persistence.ErrAnchorDigest) {
		t.Fatalf("CheckAnchor over a rewritten batch: got %v, want ErrAnchorDigest", err)
	}
}

// Scenario: a batch before the anchored one is rewritten
//
// ADR-015 claims alteration anywhere within the first N batches is detected.
// The previous test only rewrites batch N itself, so it does not establish
// that. This does.
func TestAnchorDetectsARewrittenEarlierBatch(t *testing.T) {
	path, _ := twoBatches(t)
	anchor := anchorOf(t, path)
	head, tail := split(t)

	if err := os.WriteFile(path, journalBytes(t, altered(t, head), tail), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if j := readFile(t, path); len(j.Batches) != 2 || j.Tail != persistence.TailComplete {
		t.Fatalf("the rebuilt journal did not read back whole: %d batches, tail %v", len(j.Batches), j.Tail)
	}

	if err := persistence.CheckAnchor(path, anchor); !errors.Is(err, persistence.ErrAnchorDigest) {
		t.Fatalf("CheckAnchor over a rewritten earlier batch: got %v, want ErrAnchorDigest", err)
	}
}

// Scenario: cut back to a boundary, then decided again
//
// The realistic attack: rewind past an act, take a different one, and arrive at
// the same batch number. It is the case that proves the two refusals
// discriminate — unreached is for a journal that stops short, and this one does
// not stop short.
func TestAnchorDetectsATruncatedJournalGrownBackAgain(t *testing.T) {
	path, afterFirst := twoBatches(t)
	anchor := anchorOf(t, path)
	_, tail := split(t)

	if err := os.Truncate(path, afterFirst); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	commit(t, path, altered(t, tail))

	err := persistence.CheckAnchor(path, anchor)
	if errors.Is(err, persistence.ErrAnchorUnreached) {
		t.Fatalf("a journal grown back to batch 2 was reported as not reaching it: %v", err)
	}
	if !errors.Is(err, persistence.ErrAnchorDigest) {
		t.Fatalf("CheckAnchor after rewind and redecide: got %v, want ErrAnchorDigest", err)
	}
}

// Scenario: a malformed anchor accuses nobody
//
// A zero anchor against a healthy journal used to produce "the journal confirms
// 2 batches, the anchor confirms 0" — a complaint about the journal for a
// defect in the reference. When the accusation is the product, that is the one
// direction the refusal must never point.
func TestAMalformedAnchorIsRefusedBeforeTheJournalIsRead(t *testing.T) {
	path, _ := twoBatches(t)
	sound := anchorOf(t, path)

	for name, bad := range map[string]persistence.Anchor{
		"zero":         {},
		"no batch":     {LastSequence: sound.LastSequence, PrefixDigest: sound.PrefixDigest},
		"no sequence":  {LastBatch: sound.LastBatch, PrefixDigest: sound.PrefixDigest},
		"short digest": {LastBatch: sound.LastBatch, LastSequence: sound.LastSequence, PrefixDigest: "abc"},
		"not hex":      {LastBatch: sound.LastBatch, LastSequence: sound.LastSequence, PrefixDigest: sound.PrefixDigest[:63] + "z"},
		// "A" and not "Z": uppercase and still a hex digit, so this fails for
		// being the second spelling of a value rather than for not being hex.
		"upper-case hex": {LastBatch: sound.LastBatch, LastSequence: sound.LastSequence,
			PrefixDigest: sound.PrefixDigest[:63] + "A"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := bad.Validate(); !errors.Is(err, persistence.ErrAnchorMalformed) {
				t.Fatalf("Validate: got %v, want ErrAnchorMalformed", err)
			}
			if err := persistence.CheckAnchor(path, bad); !errors.Is(err, persistence.ErrAnchorMalformed) {
				t.Fatalf("CheckAnchor: got %v, want ErrAnchorMalformed", err)
			}
		})
	}
}

// Scenario: two runs of one configuration differ only in the label, and an
// anchor tells them apart
//
// This is the criterion the whole identity clause exists for, as a regression.
// Two sessions of one subject over one file are byte-identical — that is a
// property four determinism tests rest on, and it is why an anchor keyed on the
// configuration could be presented against either. The run identity is the only
// thing that separates them, so the journals must differ in that and nothing
// else, and one session's anchor must be refused against the other.
func TestTwoRunsDifferOnlyByTheirIdentityAndAnAnchorTellsThemApart(t *testing.T) {
	head, tail := split(t)
	first, second := journalPath(t), journalPath(t)

	rename := func(events []session.Event, runID string) []session.Event {
		out := append([]session.Event{}, events...)
		for n, e := range out {
			if started, ok := e.(session.SessionStarted); ok {
				started.Config.RunID = runID
				out[n] = started
			}
		}
		return out
	}
	for path, runID := range map[string]string{first: "r-one", second: "r-two"} {
		commit(t, path, rename(head, runID))
		commit(t, path, tail)
	}

	one, err := os.ReadFile(first)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	other, err := os.ReadFile(second)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if bytes.Equal(one, other) {
		t.Fatal("two runs with different identities produced identical journals")
	}
	// The bytes differ by more than the label — a batch header carries a
	// checksum over its payload — so the claim is made where it is true: the
	// two recorded the same history, and the only thing that separates them is
	// the name of the run.
	sameExcept := func(path string) []session.Event {
		events := readFile(t, path).Events()
		for n, e := range events {
			if started, ok := e.(session.SessionStarted); ok {
				started.Config.RunID = ""
				events[n] = started
			}
		}
		return events
	}
	if !reflect.DeepEqual(sameExcept(first), sameExcept(second)) {
		t.Fatal("the two journals differ in more than the run they name")
	}

	anchor := anchorOf(t, first)
	if anchor.RunID != "r-one" {
		t.Fatalf("the anchor names run %q, want the one the journal names", anchor.RunID)
	}
	if err := persistence.CheckAnchor(first, anchor); err != nil {
		t.Fatalf("CheckAnchor against its own run: %v", err)
	}
	if err := persistence.CheckAnchor(second, anchor); !errors.Is(err, persistence.ErrAnchorRun) {
		t.Fatalf("one run's anchor against another: got %v, want ErrAnchorRun", err)
	}
}

// Scenario: a journal written before identity existed makes no claim about it
//
// v4 journals stay readable, and an anchor cannot settle an identity clause
// against one. It says so rather than passing, which is the rule the whole
// document is about.
func TestAnAnchorCannotSettleIdentityAgainstAJournalThatNamesNone(t *testing.T) {
	head, _ := split(t)
	path := journalPath(t)

	scripted := append([]session.Event{}, head...)
	for n, e := range scripted {
		if started, ok := e.(session.SessionStarted); ok {
			started.Config.SubjectID, started.Config.RunID = "", ""
			started.Config.Pacing = session.PacingScripted
			scripted[n] = started
		}
	}
	framed, err := persistence.EncodeBatch(1, scripted, persistence.EventVersionV4)
	if err != nil {
		t.Fatalf("EncodeBatch: %v", err)
	}
	if err := os.WriteFile(path, append(persistence.HeaderFor(persistence.EventVersionV4), framed...), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := persistence.RunIdentity(path); !errors.Is(err, persistence.ErrNoRunIdentity) {
		t.Fatalf("RunIdentity of a v4 journal: got %v, want ErrNoRunIdentity", err)
	}
	named := anchorOf(t, path)
	named.RunID = "r-one"
	if err := persistence.CheckAnchor(path, named); !errors.Is(err, persistence.ErrNoRunIdentity) {
		t.Fatalf("CheckAnchor naming a run against a journal that names none: got %v, want ErrNoRunIdentity", err)
	}
}

// Scenario: a journal names the run it is
func TestAJournalNamesTheRunItIs(t *testing.T) {
	path, _ := twoBatches(t)
	identity, err := persistence.RunIdentity(path)
	if err != nil {
		t.Fatalf("RunIdentity: %v", err)
	}
	if identity != "r-01" {
		t.Fatalf("RunIdentity: got %q, want the label the fixture supplied", identity)
	}
}

// Scenario: two runs of one configuration are byte-identical// Scenario: two runs of one configuration are byte-identical
//
// The demonstration behind ErrNoRunIdentity, kept as a test so the reason the
// clause cannot be honoured is a fact in the suite rather than an assertion in
// a document. When v5 gives a run its own identity this stops being true, and
// that is the point.
func TestTwoRunsOfOneConfigurationAreIndistinguishable(t *testing.T) {
	head, tail := split(t)

	first, second := journalPath(t), journalPath(t)
	for _, path := range []string{first, second} {
		commit(t, path, head)
		commit(t, path, tail)
	}

	a, b := anchorOf(t, first), anchorOf(t, second)
	if a != b {
		t.Skipf("the fixture now distinguishes two runs, so the anchor could bind one:\n %+v\n %+v", a, b)
	}
	if err := persistence.CheckAnchor(second, a); err != nil {
		t.Fatalf("an anchor from one run failed against another it cannot be told from: %v", err)
	}
}

// Scenario: an anchor does not depend on when it was taken
func TestAnchorIsStableAcrossReads(t *testing.T) {
	path, _ := twoBatches(t)
	first, second := anchorOf(t, path), anchorOf(t, path)
	if first != second {
		t.Fatalf("anchors differ across reads:\n %+v\n %+v", first, second)
	}
}

// Scenario: the writer's anchor and the file's are one claim
//
// Two paths to one value drift, and this one exists only because the other
// cannot be called often enough. So the equivalence is the property that has to
// hold, and it is checked at every batch rather than at the end: the first pass
// is a writer that created the file, the second one that inherited it, which is
// the seeding the two paths could most easily disagree about.
func TestTheWritersAnchorMatchesTheFileFreshAndResumed(t *testing.T) {
	head, tail := split(t)
	path := journalPath(t)

	for n, events := range [][]session.Event{head, tail} {
		w, err := persistence.OpenWriter(path, persistence.DurableEveryBatch)
		if err != nil {
			t.Fatalf("batch %d: OpenWriter: %v", n+1, err)
		}
		if _, err := w.Append(events); err != nil {
			w.Close()
			t.Fatalf("batch %d: Append: %v", n+1, err)
		}
		fromWriter, err := w.Anchor()
		if err != nil {
			w.Close()
			t.Fatalf("batch %d: Anchor: %v", n+1, err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("batch %d: Close: %v", n+1, err)
		}

		if fromFile := anchorOf(t, path); fromWriter != fromFile {
			t.Fatalf("batch %d: the writer and the file disagree\n writer: %+v\n file:   %+v",
				n+1, fromWriter, fromFile)
		}
		if err := persistence.CheckAnchor(path, fromWriter); err != nil {
			t.Fatalf("batch %d: CheckAnchor against the writer's own anchor: %v", n+1, err)
		}
	}
}

// Scenario: a writer holding the lock refuses a reader taking an anchor
//
// This is why the writer has to be able to produce one. ADR-015 says an anchor
// is published before the command that produced it is reported successful, and
// at that moment the lock is held — so the reading path cannot be the one that
// honours the ordering, by the same policy that keeps a moving tail from being
// read.
func TestAnchorOfIsRefusedWhileAWriterHoldsTheJournal(t *testing.T) {
	head, _ := split(t)
	path := journalPath(t)

	w, err := persistence.OpenWriter(path, persistence.DurableEveryBatch)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	defer w.Close()
	if _, err := w.Append(head); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if _, err := persistence.AnchorOf(path); err == nil {
		t.Fatal("AnchorOf read a journal that a writer was holding")
	}
	if _, err := w.Anchor(); err != nil {
		t.Fatalf("the writer could not anchor its own journal: %v", err)
	}
}

// Scenario: a writer that has confirmed nothing has no anchor
func TestAWriterWithNoConfirmedBatchHasNoAnchor(t *testing.T) {
	path := journalPath(t)
	w, err := persistence.OpenWriter(path, persistence.DurableEveryBatch)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	defer w.Close()

	if _, err := w.Anchor(); !errors.Is(err, persistence.ErrEmptyBatch) {
		t.Fatalf("Anchor on a journal with no batch: got %v, want ErrEmptyBatch", err)
	}
}

// Scenario: the report carries the events its claims were made over
//
// A caller checking them against a market file checks the events that were
// proved, not a second read of the journal — which could be a different file.
func TestProveAgainstReportsTheEventsItProved(t *testing.T) {
	path, _ := twoBatches(t)
	report, err := persistence.ProveAgainst(path, nil)
	if err != nil {
		t.Fatalf("ProveAgainst: %v", err)
	}
	if want := readFile(t, path).Events(); !reflect.DeepEqual(report.Events, want) {
		t.Fatalf("the report carries %d events, the journal %d", len(report.Events), len(want))
	}
}

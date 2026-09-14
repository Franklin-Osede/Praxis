package persistence

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"praxis/internal/challenge"
	"praxis/internal/market"
	"praxis/internal/session"
)

var errInjected = errors.New("injected failure")

func twoEvents() []session.Event {
	return []session.Event{
		SessionStartedFixture(1),
		session.SessionOpened{
			Envelope:  session.Envelope{Time: 2_000, Sequence: 2, Kind: session.KindSessionOpened},
			SessionID: "d1", BalanceCts: 5_000_000, EquityCts: 5_000_000,
		},
	}
}

// SessionStartedFixture keeps the internal tests independent of the external
// fixture file, which lives in the other test package.
func SessionStartedFixture(sequence uint64) session.Event {
	return session.SessionStarted{
		Envelope: session.Envelope{Time: 1_000, Sequence: sequence, Kind: session.KindSessionStarted},
		Config: session.Config{
			Instrument:               market.Instrument{Symbol: "MNQ", CentsPerTick: 50},
			StartingBalanceCts:       5_000_000,
			CommissionPerContractCts: 50,
			Rules: challenge.Rules{
				StartingBalanceCts: 5_000_000,
				MaxDailyLossCts:    100_000,
			},
		},
	}
}

func tempJournal(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "journal.praxis")
}

// Scenario: a sync that fails poisons the writer
//
//	Given an append whose bytes reached the operating system
//	When the sync fails
//	Then the append is not confirmed and the writer refuses to continue,
//	  because the batch on disk may be whole or partial and only reading the
//	  journal can tell.
func TestAFailedSyncPoisonsTheWriter(t *testing.T) {
	path := tempJournal(t)
	w, err := OpenWriter(path, DurableEveryBatch)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	defer w.Close()

	original := syncFile
	syncFile = func(f *os.File) error { return errInjected }
	defer func() { syncFile = original }()

	if _, err := w.Append(twoEvents()); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("error: got %v, want %v", err, ErrPoisoned)
	}

	syncFile = original
	if _, err := w.Append(twoEvents()); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("a poisoned writer accepted an append: %v", err)
	}
	if w.NextBatchNumber() != 1 {
		t.Fatalf("a poisoned writer advanced to batch %d", w.NextBatchNumber())
	}
}

// Scenario: a write that fails part way leaves a tail the reader can recover
// from, and a writer that will not continue
func TestAPartialWritePoisonsAndLeavesARecoverableTail(t *testing.T) {
	path := tempJournal(t)
	w, err := OpenWriter(path, DurableEveryBatch)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	if _, err := w.Append(twoEvents()); err != nil {
		t.Fatalf("Append: %v", err)
	}

	original := writeFile
	writeFile = func(f *os.File, b []byte) (int, error) {
		half := len(b) / 2
		n, _ := f.Write(b[:half])
		return n, errInjected
	}
	next := []session.Event{
		session.SessionEnded{
			Envelope:  session.Envelope{Time: 3_000, Sequence: 3, Kind: session.KindSessionEnded},
			SessionID: "d1",
		},
	}
	_, err = w.Append(next)
	writeFile = original
	if !errors.Is(err, ErrPoisoned) {
		t.Fatalf("error: got %v, want %v", err, ErrPoisoned)
	}
	w.Close()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	j, err := ReadJournal(f)
	if err != nil {
		t.Fatalf("ReadJournal: %v", err)
	}
	if len(j.Batches) != 1 {
		t.Fatalf("batches: got %d, want the confirmed one", len(j.Batches))
	}
	if j.Tail != TailIncomplete || j.DiscardedBytes == 0 {
		t.Fatalf("tail: got %v with %d bytes discarded", j.Tail, j.DiscardedBytes)
	}

	// And the journal is not written on top of until it is repaired.
	if _, err := OpenWriter(path, DurableEveryBatch); !errors.Is(err, ErrUnconfirmedTail) {
		t.Fatalf("reopening: got %v, want %v", err, ErrUnconfirmedTail)
	}
}

// A failure while creating a journal does not leave a half-made one behind
// that a later open would accept.
func TestAFailureWhileCreatingLeavesNothingUsable(t *testing.T) {
	path := tempJournal(t)

	original := syncDir
	syncDir = func(string) error { return errInjected }
	_, err := OpenWriter(path, DurableEveryBatch)
	syncDir = original

	if !errors.Is(err, errInjected) {
		t.Fatalf("error: got %v, want the injected failure", err)
	}
}

// A short write that reports no error is still a failure. os.File never does
// this, but the check is what makes that a property of the writer rather than
// an assumption about one implementation of Write.
func TestAShortWriteWithoutAnErrorIsStillAFailure(t *testing.T) {
	path := tempJournal(t)
	w, err := OpenWriter(path, DurableEveryBatch)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	defer w.Close()

	original := writeFile
	writeFile = func(f *os.File, b []byte) (int, error) {
		return f.Write(b[:len(b)/2])
	}
	_, err = w.Append(twoEvents())
	writeFile = original

	if !errors.Is(err, ErrPoisoned) {
		t.Fatalf("error: got %v, want %v", err, ErrPoisoned)
	}
	if _, err := w.Append(twoEvents()); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("a poisoned writer accepted an append: %v", err)
	}
}

// batchTwo continues twoEvents, so a test can confirm one batch and then fail
// on the next. The writer checks sequence continuity and not domain sense.
func batchTwo() []session.Event {
	return []session.Event{
		session.SessionOpened{
			Envelope:  session.Envelope{Time: 3_000, Sequence: 3, Kind: session.KindSessionOpened},
			SessionID: "d2", BalanceCts: 5_000_000, EquityCts: 5_000_000,
		},
	}
}

// Scenario: a poisoned writer hands out no anchor
//
//	Given a writer that confirmed one batch and then failed to sync the next
//	When an anchor is asked for
//	Then it is refused, because the writer can no longer say what is on disk
//	  and a digest that cannot be said to cover the file is a reference to
//	  bytes nothing vouches for — which is the one thing an anchor is for.
func TestAPoisonedWriterHandsOutNoAnchor(t *testing.T) {
	path := tempJournal(t)
	w, err := OpenWriter(path, DurableEveryBatch)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	defer w.Close()

	if _, err := w.Append(twoEvents()); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := w.Anchor(); err != nil {
		t.Fatalf("Anchor after a confirmed batch: %v", err)
	}

	original := syncFile
	syncFile = func(f *os.File) error { return errInjected }
	if _, err := w.Append(batchTwo()); !errors.Is(err, ErrPoisoned) {
		syncFile = original
		t.Fatalf("Append: got %v, want ErrPoisoned", err)
	}
	syncFile = original

	if _, err := w.Anchor(); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("a poisoned writer produced an anchor: %v", err)
	}
}

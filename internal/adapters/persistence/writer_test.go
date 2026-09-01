package persistence_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"praxis/internal/adapters/persistence"
	"praxis/internal/session"
)

func openWriter(t *testing.T, path string) *persistence.Writer {
	t.Helper()
	w, err := persistence.OpenWriter(path, persistence.DurableEveryBatch)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	return w
}

func journalPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "journal.praxis")
}

func readFile(t *testing.T, path string) *persistence.Journal {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	j, err := persistence.ReadJournal(f)
	if err != nil {
		t.Fatalf("ReadJournal: %v", err)
	}
	return j
}

// Scenario: a journal written, closed and reopened reads back whole
func TestAppendCloseReopenAndReplay(t *testing.T) {
	path := journalPath(t)
	events := realSessionEvents(t)

	w := openWriter(t, path)
	if w.NextBatchNumber() != 1 || w.NextSequence() != 1 {
		t.Fatalf("a new journal starts at %d/%d", w.NextBatchNumber(), w.NextSequence())
	}
	if _, err := w.Append(events[:1]); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := w.Append(events[1:]); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	j := readFile(t, path)
	if j.Tail != persistence.TailComplete || len(j.Batches) != 2 {
		t.Fatalf("journal: tail %v, %d batches", j.Tail, len(j.Batches))
	}
	if !reflect.DeepEqual(j.Events(), events) {
		t.Fatal("the recovered events differ from the ones written")
	}
	if _, err := session.Replay(j.Events()); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	// Reopening continues the numbering rather than starting again.
	again := openWriter(t, path)
	if again.NextBatchNumber() != 3 {
		t.Fatalf("next batch: got %d, want 3", again.NextBatchNumber())
	}
	if again.NextSequence() != uint64(len(events))+1 {
		t.Fatalf("next sequence: got %d, want %d", again.NextSequence(), len(events)+1)
	}
}

// Scenario: creating a journal leaves a header that survives on its own
func TestCreatingAJournalWritesItsHeader(t *testing.T) {
	path := journalPath(t)
	w := openWriter(t, path)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(raw) != string(persistence.Header()) {
		t.Fatalf("file: got %q, want just the header", raw)
	}

	j := readFile(t, path)
	if len(j.Batches) != 0 || j.Tail != persistence.TailComplete {
		t.Fatalf("an empty journal reads as %d batches, tail %v", len(j.Batches), j.Tail)
	}
}

// Scenario: only one writer at a time, enforced by the kernel
//
// The test uses a real second process. Lock semantics between descriptors
// inside one process vary by platform and do not represent the conflict this
// exists to prevent.
func TestOnlyOneProcessCanWriteAJournal(t *testing.T) {
	path := journalPath(t)

	held := openWriter(t, path)
	if err := tryOpenInAnotherProcess(t, path); !errors.Is(err, errChildLocked) {
		t.Fatalf("second process: got %v, want it to be refused the lock", err)
	}

	if err := held.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := tryOpenInAnotherProcess(t, path); err != nil {
		t.Fatalf("after closing, the second process still could not open: %v", err)
	}
}

// Scenario: a lock dies with the process that held it
//
// Nothing has to notice that the owner is gone, or check whether a recorded
// process id is still alive or has been reused.
func TestALockDiesWithItsProcess(t *testing.T) {
	path := journalPath(t)

	cmd := helperCommand(t, "hold", path)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForFile(t, path)

	if _, err := persistence.OpenWriter(path, persistence.DurableEveryBatch); !errors.Is(err, persistence.ErrLocked) {
		cmd.Process.Kill()
		t.Fatalf("while held: got %v, want %v", err, persistence.ErrLocked)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	cmd.Wait()

	w, err := persistence.OpenWriter(path, persistence.DurableEveryBatch)
	if err != nil {
		t.Fatalf("after the holder died: %v", err)
	}
	w.Close()
}

// Scenario: a journal with an unconfirmed tail is not written on top of
//
//	Given a journal truncated inside its last batch
//	When a writer is opened
//	Then it refuses, because seeking back over the tail and writing on it
//	  would be a repair, and repair keeps the evidence.
func TestAWriterRefusesAnUnconfirmedTail(t *testing.T) {
	path := journalPath(t)
	events := realSessionEvents(t)

	w := openWriter(t, path)
	if _, err := w.Append(events[:1]); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := w.Append(events[1:]); err != nil {
		t.Fatalf("Append: %v", err)
	}
	w.Close()

	whole, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	firstBatchEnd := len(persistence.Header())
	framed, err := persistence.EncodeBatch(1, events[:1])
	if err != nil {
		t.Fatalf("EncodeBatch: %v", err)
	}
	firstBatchEnd += len(framed)

	for cut := firstBatchEnd + 1; cut < len(whole); cut++ {
		if err := os.WriteFile(path, whole[:cut], 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if _, err := persistence.OpenWriter(path, persistence.DurableEveryBatch); !errors.Is(err, persistence.ErrUnconfirmedTail) {
			t.Fatalf("cut at %d: got %v, want %v", cut, err, persistence.ErrUnconfirmedTail)
		}
		// And the confirmed part is still there to recover from.
		j := readFile(t, path)
		if len(j.Batches) != 1 || j.Tail != persistence.TailIncomplete {
			t.Fatalf("cut at %d: %d batches, tail %v", cut, len(j.Batches), j.Tail)
		}
	}
}

// Scenario: a batch that does not continue the journal is refused before any
// byte is written
func TestABatchOutOfSequenceIsRefusedBeforeWriting(t *testing.T) {
	path := journalPath(t)
	events := realSessionEvents(t)

	w := openWriter(t, path)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if _, err := w.Append(events[1:]); !errors.Is(err, persistence.ErrSequenceMismatch) {
		t.Fatalf("error: got %v, want %v", err, persistence.ErrSequenceMismatch)
	}
	if _, err := w.Append(nil); !errors.Is(err, persistence.ErrEmptyBatch) {
		t.Fatalf("error: got %v, want %v", err, persistence.ErrEmptyBatch)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a refused append changed the file")
	}

	// The writer is not poisoned by a refusal that never touched the file.
	if _, err := w.Append(events[:1]); err != nil {
		t.Fatalf("Append after a refusal: %v", err)
	}
}

func TestAnUnknownDurabilityPolicyIsRefused(t *testing.T) {
	if _, err := persistence.OpenWriter(journalPath(t), 0); !errors.Is(err, persistence.ErrUnknownPolicy) {
		t.Fatalf("error: got %v, want %v", err, persistence.ErrUnknownPolicy)
	}
}

func TestACloseWriterAcceptsNothing(t *testing.T) {
	path := journalPath(t)
	w := openWriter(t, path)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := w.Append(realSessionEvents(t)[:1]); !errors.Is(err, persistence.ErrClosed) {
		t.Fatalf("error: got %v, want %v", err, persistence.ErrClosed)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing twice: %v", err)
	}
}

// --- the auxiliary process -------------------------------------------------

var errChildLocked = errors.New("child was refused the lock")

func helperCommand(t *testing.T, mode, path string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestJournalLockHelper")
	cmd.Env = append(os.Environ(), "PRAXIS_LOCK_HELPER="+mode, "PRAXIS_LOCK_PATH="+path)
	return cmd
}

func tryOpenInAnotherProcess(t *testing.T, path string) error {
	t.Helper()
	out, err := helperCommand(t, "try", path).CombinedOutput()
	if err == nil {
		return nil
	}
	if bytes.Contains(out, []byte("LOCKED")) {
		return errChildLocked
	}
	return errors.New(string(out))
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	for n := 0; n < 400; n++ {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the helper process never created the journal")
}

// TestJournalLockHelper is the auxiliary process, not a test. It runs only
// when the environment asks for it.
func TestJournalLockHelper(t *testing.T) {
	mode := os.Getenv("PRAXIS_LOCK_HELPER")
	if mode == "" {
		t.Skip("not the helper process")
	}
	path := os.Getenv("PRAXIS_LOCK_PATH")

	w, err := persistence.OpenWriter(path, persistence.DurableEveryBatch)
	if errors.Is(err, persistence.ErrLocked) {
		os.Stdout.WriteString("LOCKED\n")
		os.Exit(3)
	}
	if err != nil {
		os.Stdout.WriteString("ERROR " + err.Error() + "\n")
		os.Exit(4)
	}
	if mode == "hold" {
		time.Sleep(2 * time.Minute) // hold the lock until killed
	}
	w.Close()
}

package persistence

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"praxis/internal/session"
)

// DurabilityPolicy says what a confirmed append guarantees. Integrity — length
// and checksum — says whether the bytes are the bytes that were written.
// Durability says whether they survived the process or the machine dying, and
// only an explicit sync answers that.
type DurabilityPolicy uint8

const (
	// DurableEveryBatch syncs the file before an append is reported
	// confirmed. It is the only policy: a buffered mode would be added when a
	// measured cost calls for one, not in advance of a consumer.
	DurableEveryBatch DurabilityPolicy = iota + 1
)

// Errors reported by a writer.
var (
	ErrLocked           = errors.New("persistence: journal already has a writer")
	ErrLockUnsupported  = errors.New("persistence: advisory locking is unavailable on this platform")
	ErrUnconfirmedTail  = errors.New("persistence: journal ends in an unconfirmed or damaged batch; repair it explicitly")
	ErrPoisoned         = errors.New("persistence: writer failed mid-append and cannot continue")
	ErrClosed           = errors.New("persistence: writer is closed")
	ErrUnknownPolicy    = errors.New("persistence: unknown durability policy")
	ErrSequenceMismatch = errors.New("persistence: batch does not continue the journal's sequence")
)

// writeFile, syncFile and syncDir are seams so a failing write or sync can be
// exercised, which is otherwise not reachable from a test. Nothing outside
// this package can replace them.
var (
	writeFile = func(f *os.File, b []byte) (int, error) { return f.Write(b) }
	syncFile  = func(f *os.File) error { return f.Sync() }
	syncDir   = func(path string) error {
		d, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		defer d.Close()
		return d.Sync()
	}
)

// Writer appends batches to a journal, one writer at a time.
//
// The advisory lock is held from open to close, not taken per batch: a window
// between two batches in which another process could append would defeat the
// point of holding it at all.
type Writer struct {
	file   *os.File
	path   string
	policy DurabilityPolicy

	nextNumber   uint64
	nextSequence uint64

	// poisoned records a failure that left the file in a state this writer
	// cannot reason about. It never clears.
	poisoned error
	closed   bool
}

// OpenWriter opens a journal for appending, creating it if it does not exist.
//
// It refuses a journal that does not end on a batch boundary. Seeking back
// over an unconfirmed tail and writing on top of it would be a repair, and
// repair is an explicit operation that keeps the evidence.
func OpenWriter(path string, policy DurabilityPolicy) (*Writer, error) {
	if policy != DurableEveryBatch {
		return nil, fmt.Errorf("%w: %d", ErrUnknownPolicy, policy)
	}

	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := lockExclusive(file); err != nil {
		file.Close()
		return nil, err
	}

	w := &Writer{file: file, path: path, policy: policy, nextNumber: 1, nextSequence: 1}

	info, err := file.Stat()
	if err != nil {
		w.abandon()
		return nil, err
	}

	if info.Size() == 0 {
		if err := w.writeAll(Header()); err != nil {
			w.abandon()
			return nil, err
		}
		if err := syncFile(file); err != nil {
			w.abandon()
			return nil, err
		}
		// The directory entry of a newly created file needs its own sync, or
		// the file itself can survive while its name does not. This belongs to
		// creation, not to every batch.
		if err := syncDir(path); err != nil {
			w.abandon()
			return nil, err
		}
		return w, nil
	}

	if _, err := file.Seek(0, 0); err != nil {
		w.abandon()
		return nil, err
	}
	journal, err := ReadJournal(file)
	if err != nil {
		w.abandon()
		return nil, err
	}
	if journal.Tail != TailComplete {
		w.abandon()
		return nil, fmt.Errorf("%w: %v tail of %d bytes", ErrUnconfirmedTail, journal.Tail, journal.DiscardedBytes)
	}
	if n := len(journal.Batches); n > 0 {
		w.nextNumber = journal.Batches[n-1].Number + 1
		w.nextSequence = journal.Batches[n-1].LastSequence + 1
	}
	if _, err := file.Seek(0, 2); err != nil {
		w.abandon()
		return nil, err
	}
	return w, nil
}

func (w *Writer) NextBatchNumber() uint64 { return w.nextNumber }
func (w *Writer) NextSequence() uint64    { return w.nextSequence }

// Append writes one command's events as a batch and confirms it.
//
// Confirmed means the batch was validated against the last confirmed one, all
// of its bytes were written, and the file was synced. A failure at any point
// after the first byte leaves the writer poisoned: the result on disk may be a
// complete batch or a partial one, and only reading the journal can tell,
// which is why this writer refuses to guess and carry on.
func (w *Writer) Append(events []session.Event) (Batch, error) {
	switch {
	case w.closed:
		return Batch{}, ErrClosed
	case w.poisoned != nil:
		return Batch{}, fmt.Errorf("%w: %v", ErrPoisoned, w.poisoned)
	case len(events) == 0:
		return Batch{}, ErrEmptyBatch
	}

	for n, e := range events {
		if got, want := e.Header().Sequence, w.nextSequence+uint64(n); got != want {
			return Batch{}, fmt.Errorf("%w: event %d carries sequence %d, want %d", ErrSequenceMismatch, n, got, want)
		}
	}

	framed, err := EncodeBatch(w.nextNumber, events)
	if err != nil {
		return Batch{}, err
	}

	if err := w.writeAll(framed); err != nil {
		w.poison(err)
		return Batch{}, fmt.Errorf("%w: %v", ErrPoisoned, err)
	}
	if err := syncFile(w.file); err != nil {
		w.poison(err)
		return Batch{}, fmt.Errorf("%w: %v", ErrPoisoned, err)
	}

	batch := Batch{
		Number:        w.nextNumber,
		FirstSequence: events[0].Header().Sequence,
		LastSequence:  events[len(events)-1].Header().Sequence,
		Events:        events,
	}
	w.nextNumber++
	w.nextSequence = batch.LastSequence + 1
	return batch, nil
}

// Close releases the lock. The kernel would release it anyway if the process
// died, which is the point of using one.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	return w.file.Close()
}

func (w *Writer) writeAll(b []byte) error {
	n, err := writeFile(w.file, b)
	if err != nil {
		return err
	}
	if n != len(b) {
		return fmt.Errorf("wrote %d of %d bytes", n, len(b))
	}
	return nil
}

func (w *Writer) poison(err error) { w.poisoned = err }

func (w *Writer) abandon() {
	w.closed = true
	w.file.Close()
}

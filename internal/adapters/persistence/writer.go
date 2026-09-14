package persistence

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
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

	// payloadVersion is the version this journal is written in: the one it
	// already carries when it existed, and the current one when it did not. A
	// journal is never rewritten into a newer schema by being appended to.
	payloadVersion string

	// recovered is the journal this writer read while opening. It is kept so
	// that recovery and appending share one lock: reopening to read would
	// leave a window in which another process could take it.
	recovered *Journal

	// digest is the running SHA-256 of every confirmed byte, from the version
	// line onwards, and it is what makes an anchor cost nothing.
	//
	// The alternative is re-reading the journal to take one, which is O(n) a
	// call and therefore quadratic in the journal's own length at the cadence
	// ADR-015 asks for — the same shape this repository already paid for once
	// and wrote down in session/journal.go. It also cannot work: an anchor is
	// due at the moment a batch is confirmed, and this writer holds the lock
	// then, so a reader taking one would be refused by the very policy that
	// keeps a moving tail from being read.
	digest hash.Hash

	// anchoredBatch and anchoredSequence are where the digest currently stands.
	// They are the last confirmed batch, not the next one.
	anchoredBatch    uint64
	anchoredSequence uint64

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

	w := &Writer{file: file, path: path, policy: policy, nextNumber: 1, nextSequence: 1,
		payloadVersion: EventVersion, digest: sha256.New(),
		recovered: &Journal{ContainerVersion: ContainerVersion, PayloadVersion: EventVersion}}

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
		// The version line is confirmed, so it is part of the digest. An anchor
		// covers a journal from its first byte, not from the first batch.
		w.digest.Write(Header())
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
	w.recovered = journal
	w.payloadVersion = journal.PayloadVersion
	if n := len(journal.Batches); n > 0 {
		last := journal.Batches[n-1]
		w.nextNumber = last.Number + 1
		w.nextSequence = last.LastSequence + 1
		w.anchoredBatch, w.anchoredSequence = last.Number, last.LastSequence
	}

	// The digest is seeded from the prefix this writer inherited, so an anchor
	// taken after the next append covers the whole journal rather than only
	// what this process added. It is one extra sequential read, once, at open:
	// that is the difference between paying O(n) per session and paying it per
	// batch.
	if _, err := file.Seek(0, 0); err != nil {
		w.abandon()
		return nil, err
	}
	if _, err := io.CopyN(w.digest, file, journal.ConfirmedBytes); err != nil {
		w.abandon()
		return nil, err
	}

	if _, err := file.Seek(0, 2); err != nil {
		w.abandon()
		return nil, err
	}
	return w, nil
}

// Recovered is the journal this writer found when it opened, read under the
// same lock it still holds.
func (w *Writer) Recovered() *Journal { return w.recovered }

// PayloadVersion is the schema this journal is written in.
func (w *Writer) PayloadVersion() string { return w.payloadVersion }

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

	framed, err := EncodeBatch(w.nextNumber, events, w.payloadVersion)
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
	// Only now. The digest stands for confirmed bytes, and a batch is confirmed
	// once it is written and synced — so a failed sync above leaves the writer
	// poisoned with a digest that still describes the last batch that was.
	w.digest.Write(framed)
	w.anchoredBatch, w.anchoredSequence = batch.Number, batch.LastSequence

	w.nextNumber++
	w.nextSequence = batch.LastSequence + 1
	return batch, nil
}

// Anchor is the anchor for the last confirmed batch, at no cost.
//
// This is where an anchor comes from when one is published at cadence: the
// digest is already standing at the byte the last batch ended on, so taking it
// costs a Sum and no I/O, and it can be taken while this writer holds the lock
// — which is the moment ADR-015 says the anchor is due.
//
// AnchorOf reads a file and produces the same value. That it must is the
// property worth testing, and it is: two paths to one claim drift, and this one
// exists precisely because the other cannot be called often enough.
func (w *Writer) Anchor() (Anchor, error) {
	switch {
	case w.closed:
		return Anchor{}, ErrClosed
	case w.poisoned != nil:
		// A poisoned writer cannot say what is on disk, so it cannot say what a
		// digest covers. Handing one out would be a reference to bytes nothing
		// vouches for.
		return Anchor{}, fmt.Errorf("%w: %v", ErrPoisoned, w.poisoned)
	case w.anchoredBatch == 0:
		return Anchor{}, fmt.Errorf("%w: nothing is confirmed, so there is nothing to anchor", ErrEmptyBatch)
	}
	return Anchor{
		LastBatch:    w.anchoredBatch,
		LastSequence: w.anchoredSequence,
		PrefixDigest: hex.EncodeToString(w.digest.Sum(nil)),
	}, nil
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

// Commit satisfies session.BatchCommitter. The session hands over one
// command's events; batch numbers, checksums and durability stay here.
func (w *Writer) Commit(events []session.Event) error {
	_, err := w.Append(events)
	return err
}

// Recover reads a journal's confirmed batches and rebuilds the session state
// they describe.
//
// It answers the question a failed commit leaves open. If the batch reached
// the disk, the command committed and the recovered state contains it; if it
// did not, the state is the one before the command. Either way the command is
// never run again — a duplicate would be indistinguishable from a decision the
// trader made twice.
//
// An unconfirmed or damaged tail is reported, not repaired, so a caller can
// tell an interrupted append from a healthy journal.
func Recover(path string) (*session.ReplayedState, *Journal, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	journal, err := ReadJournal(f)
	if err != nil {
		return nil, nil, err
	}
	state, err := session.Replay(journal.Events())
	if err != nil {
		return nil, journal, err
	}
	return state, journal, nil
}

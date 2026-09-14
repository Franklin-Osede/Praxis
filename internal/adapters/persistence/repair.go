package persistence

import (
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"os"

	"praxis/internal/session"
)

// Condition is what a journal needs, if anything.
type Condition uint8

const (
	// ConditionClean: the journal ends on a batch boundary.
	ConditionClean Condition = iota

	// ConditionIncompleteTail: the last batch was cut short. Those bytes were
	// never confirmed — the command that produced them returned an error and
	// its session stopped — so discarding them loses nothing anyone believed
	// had happened.
	ConditionIncompleteTail

	// ConditionCorruptLastBatch: the last batch is whole on disk but fails its
	// checksum. These bytes were fully written, so the commit may have been
	// reported as confirmed and the session may have carried on. Discarding
	// them can lose a command the system considered done, which is a
	// different act from cleaning up an unfinished write.
	ConditionCorruptLastBatch

	// ConditionFatal: damage a conservative repair will not touch.
	ConditionFatal
)

func (c Condition) String() string {
	switch c {
	case ConditionIncompleteTail:
		return "incomplete tail"
	case ConditionCorruptLastBatch:
		return "corrupt last batch"
	case ConditionFatal:
		return "not repairable"
	default:
		return "clean"
	}
}

// Report is what inspection found, and what a repair would or did do.
type Report struct {
	Path             string
	ContainerVersion string
	PayloadVersion   string

	ConfirmedBatches    int
	LastSequence        uint64
	LastConfirmedOffset int64

	Tail           TailStatus
	DiscardedBytes int64
	Condition      Condition

	// CorruptBatch is set when Condition is ConditionCorruptLastBatch.
	CorruptBatch *TailBatch

	// Proved and EventsProved are set by Prove. A report without them says
	// only that the bytes are intact, which is a weaker claim than most
	// readers will take "clean" to mean.
	Proved       bool
	EventsProved int

	// ConfigDigest is set by Prove. Everything a journal proves, it proves
	// relative to its configuration, and the configuration is an axiom nothing
	// inside can check. This is what a pre-registration records beforehand so
	// that the axiom can be confirmed from outside.
	ConfigDigest string
	SubjectID    string

	// Anchor is the completeness claim, set only when one was asked for. It is
	// separate from Proved because they are separate claims over the same
	// bytes: a journal can be coherent and still not be the run an anchor
	// names, and a reader that could not tell them apart is the failure
	// ADR-015 exists to stop.
	Anchor *AnchorClaim

	// Events are the confirmed events the claims above were made over, set by
	// Prove. A caller checking them against a market file checks these, not a
	// second read of the journal — which could by then be a different file.
	Events []session.Event

	// Repaired and SidecarPath are set by an applied repair.
	Repaired    bool
	SidecarPath string

	// Detail carries anything an operator needs told in words.
	Detail string
}

// AnchorClaim is what checking a journal against an anchor found. Err is nil
// when the journal is the run the anchor confirms, as far as the anchor reaches.
type AnchorClaim struct {
	Want Anchor
	Err  error
}

// Errors reported by inspection and repair.
var (
	ErrNotAJournal      = errors.New("persistence: not a journal")
	ErrNotRepairable    = errors.New("persistence: damage is not repairable by truncation")
	ErrCorruptBatchHeld = errors.New("persistence: discarding a fully written batch needs explicit consent")
	ErrEvidenceDiffers  = errors.New("persistence: preserved evidence already exists and differs")

	// ErrNotProvable reports a journal whose frames are intact and whose
	// history is not. Checksums say the bytes are the bytes that were written;
	// they say nothing about whether what was written could have happened.
	ErrNotProvable = errors.New("persistence: the journal's frames are intact but its history cannot be proved")
)

// RepairOptions says how far a repair may go. Both default to refusing.
type RepairOptions struct {
	// Apply performs the repair. Without it, repair is a dry run that still
	// takes the exclusive lock, so what it reports describes a journal nobody
	// is writing to.
	Apply bool

	// DiscardCorruptBatch consents to losing a batch that was fully written.
	// An incomplete tail does not need it.
	DiscardCorruptBatch bool
}

// Inspect reads a journal under a shared lock and never modifies it.
//
// It refuses a journal a writer holds. Reading confirmed batches while another
// process appends is perfectly safe — they are append-only and never change —
// but the tail is exactly what is moving, so a report about it would describe
// a photograph of something mid-flight.
func Inspect(path string) (*Report, error) {
	f, err := openLocked(path, false)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return describe(path, f)
}

// Prove reads a journal under a shared lock and rebuilds every derived fact in
// it from the facts that caused it.
//
// Inspection checks frames: lengths, checksums, continuity. That is a much
// weaker claim than it sounds, and one an operator will overread. A forged
// realised amount, re-checksummed, passes inspection perfectly — the bytes are
// exactly the bytes that were written. What catches it is replaying the journal
// against the account and the evaluation that would have had to produce it,
// which ADR-012 names as the actual defence against a fabricated history.
//
// This is that defence, offered as an operation rather than only as a side
// effect of resuming a run.
func Prove(path string) (*Report, error) { return ProveAgainst(path, nil) }

// ProveAgainst is Prove plus the completeness claim, both taken under the one
// lock this already holds.
//
// The two are together and not chained for the reason OpenWriter gives for
// reading and locking together: reading twice leaves a window, and the
// participant who can cut a journal is in ADR-015's threat model. A report
// whose coherence line described one file and whose anchor line described
// another would be false by composition while neither line was false alone.
//
// A nil anchor asks for the first claim only, and the report then says nothing
// about the second rather than implying it.
func ProveAgainst(path string, want *Anchor) (*Report, error) {
	f, err := openLocked(path, false)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	report, err := describe(path, f)
	if err != nil {
		return report, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return report, err
	}
	raw, err := wholeFile(f)
	if err != nil {
		return report, err
	}
	journal, err := ReadJournal(bytes.NewReader(raw))
	if err != nil {
		return report, err
	}

	// Before Verify and Replay, because the two claims are independent and the
	// early return for an unprovable history must not erase the other one.
	if want != nil {
		report.Anchor = &AnchorClaim{Want: *want, Err: checkAnchorAgainst(raw, journal, *want)}
	}

	events := journal.Events()
	report.Events = events
	report.EventsProved = len(events)
	if len(events) > 0 {
		if started, ok := events[0].(session.SessionStarted); ok {
			report.SubjectID = started.Config.SubjectID
			if report.ConfigDigest, err = ConfigDigest(started, journal.PayloadVersion); err != nil {
				return report, err
			}
		}
	}

	// Verify catches a log that contradicts itself; Replay catches one that is
	// perfectly self-consistent and still describes something no account could
	// have done. Both, in that order, because the first names the cheaper
	// fault more precisely.
	if err := session.Verify(events); err != nil {
		return report, fmt.Errorf("%w: %v", ErrNotProvable, err)
	}
	if _, err := session.Replay(events); err != nil {
		return report, fmt.Errorf("%w: %v", ErrNotProvable, err)
	}
	report.Proved = true
	return report, nil
}

// Repair truncates a journal back to its last confirmed batch, preserving the
// bytes it discards.
func Repair(path string, opts RepairOptions) (*Report, error) {
	f, err := openLocked(path, true)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	report, err := describe(path, f)
	if err != nil {
		return nil, err
	}
	switch report.Condition {
	case ConditionClean:
		return report, nil
	case ConditionFatal:
		return report, fmt.Errorf("%w: %s", ErrNotRepairable, report.Detail)
	case ConditionCorruptLastBatch:
		if !opts.DiscardCorruptBatch {
			return report, fmt.Errorf("%w: batch %d, sequences %d..%d, %d bytes",
				ErrCorruptBatchHeld, report.CorruptBatch.Number,
				report.CorruptBatch.FirstSequence, report.CorruptBatch.LastSequence, report.DiscardedBytes)
		}
	}
	if !opts.Apply {
		return report, nil
	}

	discarded, err := readAt(f, report.LastConfirmedOffset, report.DiscardedBytes)
	if err != nil {
		return report, err
	}

	// Evidence first. A crash before the truncation leaves the bytes in two
	// places, which repeating the repair resolves; a crash after a truncation
	// that had not preserved them would leave them nowhere.
	sidecar, err := preserve(path, report.LastConfirmedOffset, discarded)
	if err != nil {
		return report, err
	}
	report.SidecarPath = sidecar

	if err := f.Truncate(report.LastConfirmedOffset); err != nil {
		return report, err
	}
	if err := syncFile(f); err != nil {
		return report, err
	}

	// Nothing is declared repaired until the result has been read back whole,
	// on the descriptor already held: reopening would contend with this
	// repair's own lock.
	after, err := describe(path, f)
	if err != nil {
		return report, fmt.Errorf("the repaired journal does not read back: %w (evidence kept at %s)", err, sidecar)
	}
	if after.Condition != ConditionClean {
		return report, fmt.Errorf("the repaired journal is still %s (evidence kept at %s)", after.Condition, sidecar)
	}

	after.Repaired, after.SidecarPath = true, sidecar
	after.Detail = report.Detail
	return after, nil
}

// describe reads a journal already open and locked.
func describe(path string, f *os.File) (*Report, error) {
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	journal, err := ReadJournal(f)
	if err != nil {
		return &Report{Path: path, Condition: ConditionFatal, Detail: err.Error()},
			fmt.Errorf("%w: %v", ErrNotAJournal, err)
	}

	report := &Report{
		Path:                path,
		ContainerVersion:    journal.ContainerVersion,
		PayloadVersion:      journal.PayloadVersion,
		ConfirmedBatches:    len(journal.Batches),
		LastConfirmedOffset: journal.ConfirmedBytes,
		Tail:                journal.Tail,
		DiscardedBytes:      journal.DiscardedBytes,
		CorruptBatch:        journal.TailBatch,
	}
	if n := len(journal.Batches); n > 0 {
		report.LastSequence = journal.Batches[n-1].LastSequence
	}

	switch journal.Tail {
	case TailIncomplete:
		report.Condition = ConditionIncompleteTail
		report.Detail = fmt.Sprintf("%d bytes of an unfinished append; they were never confirmed", journal.DiscardedBytes)
	case TailCorrupt:
		report.Condition = ConditionCorruptLastBatch
		report.Detail = fmt.Sprintf(
			"batch %d is whole on disk but fails its checksum: %d bytes carrying events %d..%d, which may have been reported as confirmed",
			journal.TailBatch.Number, journal.DiscardedBytes,
			journal.TailBatch.FirstSequence, journal.TailBatch.LastSequence)
	default:
		report.Condition = ConditionClean
	}
	if report.ConfirmedBatches == 0 && report.Condition != ConditionClean {
		report.Detail += "; no batch was ever confirmed, so a repair leaves a valid empty journal, not a broken one"
	}
	return report, nil
}

// preserve writes the discarded bytes beside the journal, named for where they
// came from and what they contain, and never replaces evidence already there.
func preserve(path string, offset int64, discarded []byte) (string, error) {
	name := preservedName(path, offset, discarded)

	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		existing, readErr := os.ReadFile(name)
		if readErr != nil {
			return "", readErr
		}
		if !bytes.Equal(existing, discarded) {
			return "", fmt.Errorf("%w: %s", ErrEvidenceDiffers, name)
		}
		return name, nil // the same evidence from an earlier attempt
	}
	if err != nil {
		return "", err
	}
	defer f.Close()

	if _, err := f.Write(discarded); err != nil {
		return "", err
	}
	if err := syncFile(f); err != nil {
		return "", err
	}
	return name, syncDir(name)
}

// preservedName says where discarded bytes are kept: beside the journal, named
// for the offset they came from and a checksum of what they contain, so two
// different tails can never collide and the same tail always lands on the same
// name.
func preservedName(path string, offset int64, discarded []byte) string {
	return fmt.Sprintf("%s.tail-%d-%08x", path, offset, crc32.Checksum(discarded, castagnoli))
}

func openLocked(path string, exclusive bool) (*os.File, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%w: %s is a directory", ErrNotAJournal, path)
	}

	flags := os.O_RDONLY
	if exclusive {
		flags = os.O_RDWR
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return nil, err
	}
	if exclusive {
		err = lockExclusive(f)
	} else {
		err = lockShared(f)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func readAt(f *os.File, offset, length int64) ([]byte, error) {
	buf := make([]byte, length)
	if _, err := f.ReadAt(buf, offset); err != nil {
		return nil, err
	}
	return buf, nil
}

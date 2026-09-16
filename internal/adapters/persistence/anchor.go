package persistence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"praxis/internal/market"
	"praxis/internal/session"
)

// Errors reported when a journal and an anchor do not agree, and when the
// anchor itself is the thing at fault.
//
// They are separate because they are separate accusations, and the accusation
// is this package's product. A journal that does not reach its anchor is
// missing something; a journal that reaches it and disagrees over the bytes was
// rewritten; an anchor that is not a well formed reference accuses nobody.
var (
	// ErrAnchorUnreached reports a journal whose confirmed batches stop before
	// the one an anchor names.
	//
	// It says what was observed and not why. Truncation is the expected cause
	// and it is not the only one: an anchor taken from a longer journal reaches
	// this same branch, and until a run has an identity nothing here can tell
	// the two apart. Naming it "truncated" would be the check asserting a cause
	// it did not establish.
	ErrAnchorUnreached = errors.New("persistence: journal does not reach the batch this anchor confirms")

	// ErrAnchorDigest reports a journal that reaches the anchored batch and
	// disagrees with the anchor over the bytes leading to it. CRC32C is a
	// checksum and not a signature, so a reframed batch is internally
	// consistent; this is what refuses it.
	ErrAnchorDigest = errors.New("persistence: journal disagrees with the anchor over bytes they both cover")

	// ErrAnchorMalformed reports a reference that cannot be checked against
	// anything. A zero anchor held against a healthy journal would otherwise
	// produce a complaint about the journal for a defect in the reference.
	ErrAnchorMalformed = errors.New("persistence: this is not a well formed anchor")

	// ErrNoRunIdentity reports that which execution a journal is cannot be
	// established. Every payload version to date is one — see Anchor.RunID.
	ErrNoRunIdentity = errors.New("persistence: this journal records no run identity")

	// ErrAnchorRun reports an anchor and a journal that name different
	// executions. Under custody there is one anchor per session, so this is the
	// refusal that keeps a session's anchor from certifying another session of
	// the same subject over the same file — which, byte for byte, it otherwise
	// could.
	ErrAnchorRun = errors.New("persistence: this anchor names a different run")
)

// Anchor is what a journal is checked against: which run it is, how far that
// run had been committed, and a digest over exactly those bytes.
//
// It is a value and not a file. Where an anchor is kept, over what channel it
// is published and whether it is authenticated are custody decisions ADR-015
// leaves open, and this type is indifferent to all three — an anchor stored
// beside the journal is well formed and worthless.
//
// What it proves is a lower bound: the journal contains at least this batch,
// and its bytes up to it are these. It proves no upper bound. Nothing here can
// say a further batch never existed, so the rate at which anchors are published
// is what bounds the acts a truncation can hide.
type Anchor struct {
	// RunID names the execution, read from the journal and never supplied by a
	// caller. Under custody there is one anchor per session, so this is what
	// keeps one session's anchor from certifying another session of the same
	// subject over the same file — which, byte for byte, it otherwise could.
	//
	// It is empty for a journal written before a journal could say, and then
	// the anchor makes no claim about identity and says so rather than passing.
	RunID string

	LastBatch    uint64
	LastSequence uint64

	// PrefixDigest is the SHA-256, lowercase hex, of the journal's bytes from
	// its version line through the last byte of LastBatch.
	PrefixDigest string
}

// Validate reports why this is not a reference anything can be checked against,
// or nil. Anchors validate themselves for the reason every other domain value
// in this tree does, and with more at stake: an unvalidated reference turns a
// defect in the evidence into a complaint about the journal.
func (a Anchor) Validate() error {
	if a.LastBatch == 0 {
		return fmt.Errorf("%w: it confirms no batch", ErrAnchorMalformed)
	}
	if a.LastSequence == 0 {
		return fmt.Errorf("%w: batch %d confirms no sequence", ErrAnchorMalformed, a.LastBatch)
	}
	if err := validDigest(a.PrefixDigest); err != nil {
		return fmt.Errorf("%w: %v", ErrAnchorMalformed, err)
	}
	return nil
}

// errDigestShape is the shape rule itself, without a context. An anchor wraps
// it in ErrAnchorMalformed and the codec in ErrSyntax, because the same bad
// value is a different accusation in each place.
var errDigestShape = errors.New("not sixty-four lowercase hexadecimal digits")

// validDigest is one definition of how this format writes a SHA-256, used by
// the payload and by an anchor alike.
//
// Lowercase is checked rune by rune rather than left to hex.DecodeString, which
// accepts both cases. The frame header already refuses uppercase for the reason
// that applies here with more force: one value, exactly one encoding — and a
// digest is compared by equality, so a value that could be spelled two ways
// would make two equal things look unequal.
func validDigest(s string) error {
	if len(s) != 2*sha256.Size {
		return fmt.Errorf("%q is %d characters, %w", s, len(s), errDigestShape)
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("%q is %w", s, errDigestShape)
		}
	}
	return nil
}

// AnchorOf takes the anchor a journal's confirmed prefix would produce.
//
// It reads the whole file, so it is a verification tool and not something to
// call after every command. An anchor published at the cadence ADR-015 requires
// has to come from the writer that is already holding those bytes; taking one
// this way once per batch is quadratic in the journal's own length, which is a
// cost this repository has already paid once elsewhere.
//
// An unconfirmed tail is not part of the anchor. A process killed mid-append
// leaves bytes no command was told succeeded, and letting them in would make an
// anchor depend on how a crash happened to land.
func AnchorOf(path string) (Anchor, error) {
	raw, journal, err := readAll(path)
	if err != nil {
		return Anchor{}, err
	}
	if len(journal.Batches) == 0 {
		return Anchor{}, fmt.Errorf("%w: nothing is confirmed, so there is nothing to anchor", ErrEmptyBatch)
	}
	last := journal.Batches[len(journal.Batches)-1]
	return Anchor{
		// Read from the journal and never supplied: an anchor names the
		// execution the journal names. Taking it as a parameter would be A2
		// inverted — a field stamped with whatever a caller passed and checked
		// against nothing.
		RunID:        runIdentityOf(journal),
		LastBatch:    last.Number,
		LastSequence: last.LastSequence,
		PrefixDigest: digestOf(raw[:last.EndOffset]),
	}, nil
}

// runIdentityOf is the execution a journal says it is, or empty for one written
// before a journal could say.
func runIdentityOf(journal *Journal) string {
	for _, b := range journal.Batches {
		for _, e := range b.Events {
			if started, ok := e.(session.SessionStarted); ok {
				return started.Config.RunID
			}
		}
	}
	return ""
}

// CheckAnchor reports whether a journal still contains, unaltered, the prefix
// an anchor confirms.
//
// A journal longer than its anchor passes. That is the point of anchoring a
// prefix rather than a file: a checkpoint is taken while the run continues, and
// the next legitimate command must not invalidate it.
//
// This is the second of the two claims in ADR-015 and never the first. It says
// nothing about whether the history is coherent, and a caller reporting one as
// the other has published a proof it does not hold.
func CheckAnchor(path string, want Anchor) error {
	raw, journal, err := readAll(path)
	if err != nil {
		return err
	}
	return checkAnchorAgainst(raw, journal, want)
}

// checkAnchorAgainst is the comparison, over bytes a caller has already read.
//
// It exists so that Prove can make the completeness claim under the lock it is
// already holding, without a second open. Two reads mean a window, and the
// participant who can cut a journal is in the threat model — so a report whose
// two lines described two different files would be false by composition while
// neither line was false on its own. One read, one lock, two claims.
//
// And one comparison. The two entry points share this because two copies of a
// comparison drift, which is the fault this whole document is about.
func checkAnchorAgainst(raw []byte, journal *Journal, want Anchor) error {
	if err := want.Validate(); err != nil {
		return err
	}
	// The identity clause, checked at last rather than refused. A journal
	// written before v5 names no run, so an anchor that names one cannot be
	// settled against it and says so; two that disagree are two runs.
	switch identity := runIdentityOf(journal); {
	case want.RunID == identity:
	case identity == "":
		return fmt.Errorf("%w: this anchor names run %q", ErrNoRunIdentity, want.RunID)
	default:
		return fmt.Errorf("%w: this anchor names run %q and the journal is run %q",
			ErrAnchorRun, want.RunID, identity)
	}

	for _, b := range journal.Batches {
		if b.Number != want.LastBatch {
			continue
		}
		// The sequence is checked as well as the digest, so a batch rebuilt to
		// a different length is named as that rather than reported only as
		// some byte having moved.
		if b.LastSequence != want.LastSequence {
			return fmt.Errorf("%w: batch %d ends at sequence %d, the anchor confirms %d",
				ErrAnchorDigest, b.Number, b.LastSequence, want.LastSequence)
		}
		if got := digestOf(raw[:b.EndOffset]); got != want.PrefixDigest {
			return fmt.Errorf("%w: through batch %d the journal digests to %s, the anchor confirms %s",
				ErrAnchorDigest, b.Number, got, want.PrefixDigest)
		}
		return nil
	}
	return fmt.Errorf("%w: the journal confirms %d batches, the anchor confirms %d",
		ErrAnchorUnreached, len(journal.Batches), want.LastBatch)
}

// RunIdentity is which execution a journal says it is.
//
// No payload version has a field for it, so this reports the gap rather than
// answering from the configuration. ConfigDigest is the tempting answer and the
// wrong one: it identifies a configuration, so two sessions by one subject over
// one file produce the same value byte for byte, and an anchor keyed on it
// could be presented against either.
//
// When v5 records an identifier this reads it, and the test asserting this
// error is what fails to say so.
func RunIdentity(path string) (string, error) {
	_, journal, err := readAll(path)
	if err != nil {
		return "", err
	}
	identity := runIdentityOf(journal)
	if identity == "" {
		return "", ErrNoRunIdentity
	}
	return identity, nil
}

// readAll returns a journal's bytes and its parse together, under the shared
// lock Inspect and Prove take.
//
// The lock is the package's existing policy and not a new one: a journal being
// written is refused, because the tail is exactly what is moving. Confirmed
// bytes never change, so a third policy could be defended here — but the reason
// to want one is taking an anchor mid-run, and that belongs in the writer.
func readAll(path string) ([]byte, *Journal, error) {
	f, err := openLocked(path, false)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	raw, err := wholeFile(f)
	if err != nil {
		return nil, nil, err
	}
	journal, err := ReadJournal(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, err
	}
	return raw, journal, nil
}

// wholeFile reads a journal's bytes under the reader's own size bound. One
// definition, so a second reader cannot admit a file the first would refuse.
func wholeFile(f *os.File) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(f, MaxJournalBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxJournalBytes {
		return nil, fmt.Errorf("%w: journal exceeds %d bytes", ErrTooLarge, MaxJournalBytes)
	}
	return raw, nil
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// AnchorVersion is the only spelling of an anchor this build writes or reads.
//
// It is named separately from the container and payload versions because it
// changes for a third reason: an anchor is written down, carried somewhere and
// typed back, and how it is spelled has nothing to do with how a batch is
// delimited or how an event is encoded.
const AnchorVersion = "praxis.anchor.v1"

const digestAlgorithm = "sha256:"

// Format is the one spelling of this anchor:
//
//	praxis.anchor.v1 <run|-> <batch> <sequence> sha256:<64 lowercase hex>
//
// Five space-separated fields, and every one of them is the spelling this
// repository already uses: "-" for an identifier that is absent, as the codec
// writes one; canonical decimals with no leading zero, as market parses them;
// lowercase hex, as a batch header requires. A second spelling for any of them
// would be a second way to write one value, and an anchor is compared by
// equality — so two anchors that are the same have to look the same.
//
// It refuses a malformed anchor rather than producing text for it. Evidence
// that cannot be checked must not acquire a written form that looks
// authoritative: the same rule that refuses a name at the door because a
// command the record cannot hold is a command that poisons the session at
// commit time.
func (a Anchor) Format() (string, error) {
	if err := a.Validate(); err != nil {
		return "", err
	}
	if a.RunID != "" {
		if err := market.ValidIdentifier(a.RunID); err != nil {
			return "", fmt.Errorf("%w: run identity: %v", ErrAnchorMalformed, err)
		}
	}
	run := a.RunID
	if run == "" {
		run = "-"
	}
	return strings.Join([]string{
		AnchorVersion,
		run,
		market.FormatUint(a.LastBatch),
		market.FormatUint(a.LastSequence),
		digestAlgorithm + a.PrefixDigest,
	}, " "), nil
}

// ParseAnchor reads an anchor that was written down.
//
// It is the exact inverse of Format and accepts nothing else. A reader that
// tolerated a second spelling would let one anchor arrive in two forms, and the
// two would compare unequal while naming the same prefix — which for a value
// whose whole job is comparison is the failure, not an inconvenience.
func ParseAnchor(text string) (Anchor, error) {
	fields := strings.Split(text, " ")
	if len(fields) != 5 {
		return Anchor{}, fmt.Errorf("%w: %d fields, want 5", ErrAnchorMalformed, len(fields))
	}
	if fields[0] != AnchorVersion {
		return Anchor{}, fmt.Errorf("%w: %q is not %s", ErrAnchorMalformed, fields[0], AnchorVersion)
	}

	var run string
	if fields[1] != "-" {
		if err := market.ValidIdentifier(fields[1]); err != nil {
			return Anchor{}, fmt.Errorf("%w: run identity %q: %v", ErrAnchorMalformed, fields[1], err)
		}
		run = fields[1]
	}

	batch, err := market.ParseUint(fields[2])
	if err != nil {
		return Anchor{}, fmt.Errorf("%w: batch %q: %v", ErrAnchorMalformed, fields[2], err)
	}
	sequence, err := market.ParseUint(fields[3])
	if err != nil {
		return Anchor{}, fmt.Errorf("%w: sequence %q: %v", ErrAnchorMalformed, fields[3], err)
	}

	if !strings.HasPrefix(fields[4], digestAlgorithm) {
		return Anchor{}, fmt.Errorf("%w: digest %q does not name %s",
			ErrAnchorMalformed, fields[4], strings.TrimSuffix(digestAlgorithm, ":"))
	}

	// Validate carries the digest's own rules — length, and lowercase hex — so
	// they are checked in one place rather than twice with a chance to differ.
	anchor := Anchor{
		RunID: run, LastBatch: batch, LastSequence: sequence,
		PrefixDigest: strings.TrimPrefix(fields[4], digestAlgorithm),
	}
	if err := anchor.Validate(); err != nil {
		return Anchor{}, err
	}
	return anchor, nil
}

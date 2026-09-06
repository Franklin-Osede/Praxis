// Package persistence encodes domain events as canonical text and reads them
// back.
//
// It is an adapter: it owns a file format, which no domain package may. The
// codec's contract is that the same events produce exactly the same bytes, on
// every run and every machine, and that reading a canonical payload and
// writing it again reproduces it byte for byte.
//
// This package does not frame, checksum or persist anything. Length, CRC and
// durability belong to the store; see docs/adr/012-event-store-batches.md.
//
// # The grammar
//
// UTF-8, one event per line, terminated by "\n" and never by anything the host
// operating system would prefer. A line is the event's type followed by its
// fields, separated by single spaces, in a fixed order for that type.
//
// Integers are canonical decimal: no sign on positives, no spaces, no
// separators, and no leading zeros except for 0 itself. "+1", "01" and "-0"
// are refused rather than normalised, because a decoder that quietly accepted
// them would let two different files mean the same thing.
//
// Enumerations are written as names, and this package owns that mapping rather
// than borrowing a String method: a display string edited for a log message
// would otherwise change the file format.
//
// Identifiers are restricted to A-Z a-z 0-9 . _ : and -, which replaces every
// escaping question with one rule, and is enforced when writing as well as
// when reading.
//
// There is no null. A field the domain leaves at zero is written as 0, because
// the domain has already decided that zero means unset.
package persistence

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"praxis/internal/session"
)

// The event payload formats this package understands.
//
// # When a change needs a new version
//
// A version that has never been written to a real journal is a **draft**, and a
// draft may change in any way. Nothing is frozen because it was decided; it is
// frozen because bytes exist that a reader has been told how to parse. That is
// what EventVersion means: it is the version being written, and until something
// outside this repository holds those bytes, changing it costs a golden file
// and a line of the manifest.
//
// Once a version has been written, the distinction that matters is what an
// existing reader can still parse:
//
//   - adding a **field** to an existing line type is always a new version: it
//     changes bytes that reader has already been told how to parse;
//   - adding an **event type**, or a **value** to an enumeration, is also a new
//     version, because that reader has no name for it and would either refuse
//     it or, worse, guess.
//
// The distinction has been got wrong once. Inserting SourceSequence into v1's
// market_observed line was a field added to a shipped version, and the golden
// was regenerated to match rather than the change refused. The bytes v1 froze
// at f964088 no longer decode. That is recorded rather than hidden, and v1
// means what f5d12a5 left it meaning.
//
// A version refusal is not an unknown outcome. Encoding runs before the file is
// touched, so a payload this package declines to write has changed nothing and
// leaves the writer healthy — while the session treats every commit error as
// terminal, because most of them genuinely are. Today no session can produce
// one: the version written is always EventVersion, the format has no widths to
// overflow, integers are int64 in decimal, and every identifier is refused by
// the domain before it reaches here. It becomes reachable again the first time
// a migration runs two versions side by side and an event exists that one can
// express and the other cannot. Saying so here costs a paragraph and saves the
// diagnosis when it returns.
//
// **Freezing starts at the first pilot journal.** Nothing outside this
// repository has ever read a Praxis journal, so v1 through v3 are an exercise
// in discipline rather than a compatibility guarantee, and saying otherwise
// would be claiming a cost nobody has paid. From the first recorded session
// onwards the guarantee is real, and testdata/goldens.sha256 is what makes
// regenerating one cost something: changing a golden means changing two files,
// and the second is unmissable in a review.
//
// The position episode counter inaugurated v2, protection v3, and the
// cancellation reasons one-cancels-the-other execution needs inaugurate v4 —
// each published with the commands that first write it.
const (
	EventVersionV1 = "praxis.event.v1"
	EventVersionV2 = "praxis.event.v2"
	EventVersionV3 = "praxis.event.v3"
	EventVersionV4 = "praxis.event.v4"

	// EventVersion is what a new journal is written in.
	EventVersion = EventVersionV4
)

// ErrUnsupportedInVersion reports an event, a field or a value that the payload
// version in use has no way to express. A v1 journal cannot carry a losing
// trade streak, a v2 journal cannot carry protection, and a v3 journal cannot
// say that a leg was cancelled by its sibling: they honestly lack those facts
// rather than pretending to hold them.
var ErrUnsupportedInVersion = errors.New("persistence: this payload version cannot express that event")

// Limits checked before memory is reserved, so a corrupt length cannot ask for
// an allocation the process cannot survive.
const (
	MaxLineBytes    = 4 << 10
	MaxPayloadBytes = 1 << 20
)

// Errors reported for a payload that is not canonical event text.
var (
	ErrSyntax          = errors.New("persistence: line is not canonical event text")
	ErrUnknownEvent    = errors.New("persistence: unknown event type")
	ErrNotCanonicalInt = errors.New("persistence: integer is not in canonical form")
	ErrIdentifier      = errors.New("persistence: identifier uses a character the format forbids")
	ErrTooLarge        = errors.New("persistence: input exceeds the format's limits")
	ErrTrailingBytes   = errors.New("persistence: payload does not end with a complete line")
	ErrKindMismatch    = errors.New("persistence: event's kind contradicts its type")
)

// ConfigDigest is the SHA-256 of a journal's configuration, as the canonical
// bytes of the event that opens it.
//
// A journal proves every derived fact in it, and proves all of them *relative
// to* its configuration: the commission a fill was charged, the balance it
// started from, the rules it is judged against and who traded it are axioms,
// not conclusions. Nothing inside can catch a journal run with the wrong ones,
// because everything downstream is consistent with whatever they were.
//
// The answer is not a check. It is that a pre-registration records this digest
// before any session is traded, and anyone can then confirm that the journal in
// front of them was produced under the configuration that was registered. The
// canonical encoding is already the stable byte representation of the event, so
// the digest is stable for exactly as long as the payload version is.
func ConfigDigest(started session.SessionStarted, version string) (string, error) {
	line, err := encodeEvent(started, version)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(line))
	return hex.EncodeToString(sum[:]), nil
}

// EncodeEvents renders events as one canonical payload in the given version.
func EncodeEvents(events []session.Event, version string) ([]byte, error) {
	var b strings.Builder
	for n, e := range events {
		line, err := encodeEvent(e, version)
		if err != nil {
			return nil, fmt.Errorf("event %d: %w", n, err)
		}
		if len(line) > MaxLineBytes {
			return nil, fmt.Errorf("%w: event %d is %d bytes", ErrTooLarge, n, len(line))
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if b.Len() > MaxPayloadBytes {
		return nil, fmt.Errorf("%w: payload is %d bytes", ErrTooLarge, b.Len())
	}
	return []byte(b.String()), nil
}

// DecodeEvents reads a canonical payload of the given version back into events.
func DecodeEvents(payload []byte, version string) ([]session.Event, error) {
	if len(payload) > MaxPayloadBytes {
		return nil, fmt.Errorf("%w: payload is %d bytes", ErrTooLarge, len(payload))
	}
	if len(payload) == 0 {
		return nil, nil
	}
	if payload[len(payload)-1] != '\n' {
		return nil, ErrTrailingBytes
	}

	lines := strings.Split(string(payload[:len(payload)-1]), "\n")
	events := make([]session.Event, 0, len(lines))
	for n, line := range lines {
		if len(line) > MaxLineBytes {
			return nil, fmt.Errorf("%w: line %d is %d bytes", ErrTooLarge, n, len(line))
		}
		e, err := decodeEvent(line, version)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n+1, err)
		}
		events = append(events, e)
	}
	return events, nil
}

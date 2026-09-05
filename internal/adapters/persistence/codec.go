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
	"errors"
	"fmt"
	"strings"

	"praxis/internal/session"
)

// The event payload formats this package understands.
//
// A version is stable when its bytes stop changing, not when anyone promises
// the next change will be the last. v1 is what a writer and a command line have
// already been able to produce, so it keeps the schema it had; the position
// episode counter and the protection events inaugurate v2.
const (
	EventVersionV1 = "praxis.event.v1"
	EventVersionV2 = "praxis.event.v2"

	// EventVersion is what a new journal is written in.
	EventVersion = EventVersionV2
)

// ErrUnsupportedInVersion reports an event, or a field, that the payload
// version in use has no way to express. A v1 journal cannot carry protection:
// it honestly lacks those facts rather than pretending to hold them.
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

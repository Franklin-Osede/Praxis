package persistence

import (
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strings"

	"praxis/internal/session"
)

// The container's version, named separately from the payload's because the two
// change for different reasons: a frame can change its checksum, its widths or
// its recovery strategy without any event changing, and a payload can add a
// schema without changing how batches are delimited.
const ContainerVersion = "1"

// compatible is the explicit table of combinations this build understands. A
// recognised container with an unrecognised payload is refused, and so is the
// reverse.
// compatible is the explicit table of combinations this build understands. A
// recognised container with an unrecognised payload is refused, and so is the
// reverse. Both payload versions are readable; only the newer is written.
var compatible = map[string]map[string]bool{
	ContainerVersion: {EventVersionV1: true, EventVersionV2: true, EventVersionV3: true, EventVersionV4: true, EventVersionV5: true},
}

const (
	magic        = "PRAXIS-EVENT-STORE"
	batchKeyword = "BATCH"
	crcPrefix    = "CRC32C:"

	// MaxJournalBytes bounds what a reader will hold at once.
	MaxJournalBytes = 64 << 20
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Errors reported for a journal that is not a readable event store.
var (
	ErrMagic            = errors.New("persistence: not a Praxis event store")
	ErrVersionPair      = errors.New("persistence: unsupported combination of container and payload versions")
	ErrFrame            = errors.New("persistence: batch header is not well formed")
	ErrChecksum         = errors.New("persistence: batch checksum does not match its bytes")
	ErrMetadataMismatch = errors.New("persistence: batch metadata disagrees with its payload")
	ErrDiscontinuous    = errors.New("persistence: batch does not continue the one before it")
	ErrCorruptMidFile   = errors.New("persistence: a batch is damaged and more data follows it")
	ErrEmptyBatch       = errors.New("persistence: batch carries no events")
)

// Batch is one command's events, framed together.
type Batch struct {
	Number        uint64
	FirstSequence uint64
	LastSequence  uint64
	Events        []session.Event

	// EndOffset is the offset just past this batch's last byte. It exists
	// because an anchor confirms a prefix rather than a file — see ADR-015 —
	// and checking one means digesting exactly the bytes up to the batch it
	// names, which nothing outside this reader can locate.
	EndOffset int64
}

// TailStatus says how a journal ended, because the endings do not mean the
// same thing and treating them alike would hide a defect or refuse a file that
// is merely unfinished.
type TailStatus uint8

const (
	// TailComplete: the journal ends on a batch boundary.
	TailComplete TailStatus = iota

	// TailIncomplete: the last batch was cut short by end of file. The
	// ordinary shape of a process killed mid-append.
	TailIncomplete

	// TailCorrupt: the last batch is whole but its checksum is wrong.
	// Something damaged bytes that were fully written; this is not an
	// unfinished write and must not be reported as one.
	TailCorrupt
)

func (t TailStatus) String() string {
	switch t {
	case TailIncomplete:
		return "incomplete"
	case TailCorrupt:
		return "corrupt"
	default:
		return "complete"
	}
}

// TailBatch is the header of a batch that was whole on disk but failed its
// checksum. Repair reports it, because discarding it is not the same as
// discarding an unfinished write: these bytes were fully written and the
// command that produced them may have been reported as confirmed.
type TailBatch struct {
	Number        uint64
	Length        uint64
	FirstSequence uint64
	LastSequence  uint64
	EventCount    uint64
}

// Journal is what a reader recovered, and what it refused.
type Journal struct {
	ContainerVersion string
	PayloadVersion   string
	Batches          []Batch

	// Tail and DiscardedBytes describe the end of the file. A reader never
	// repairs: it reports what it disregarded and leaves the bytes alone.
	Tail           TailStatus
	DiscardedBytes int64

	// ConfirmedBytes is the offset just past the last confirmed batch, which
	// is where a repair would truncate.
	ConfirmedBytes int64

	// TailBatch is set when Tail is TailCorrupt.
	TailBatch *TailBatch
}

// Events flattens the recovered batches in order.
func (j *Journal) Events() []session.Event {
	var out []session.Event
	for _, b := range j.Batches {
		out = append(out, b.Events...)
	}
	return out
}

// EncodeBatch frames one command's events in the given payload version.
func EncodeBatch(number uint64, events []session.Event, version string) ([]byte, error) {
	if len(events) == 0 {
		return nil, ErrEmptyBatch
	}
	payload, err := EncodeEvents(events, version)
	if err != nil {
		return nil, err
	}
	first := events[0].Header().Sequence
	last := events[len(events)-1].Header().Sequence

	metadata := formatMetadata(number, uint64(len(payload)), first, last, uint64(len(events)))
	sum := crc32.Checksum(append([]byte(metadata+"\n"), payload...), castagnoli)

	var b strings.Builder
	b.WriteString(batchKeyword)
	b.WriteByte(' ')
	b.WriteString(metadata)
	b.WriteByte(' ')
	b.WriteString(crcPrefix)
	fmt.Fprintf(&b, "%08x", sum)
	b.WriteByte('\n')
	b.Write(payload)
	return []byte(b.String()), nil
}

// Header is the first line of a journal file written now.
func Header() []byte { return HeaderFor(EventVersion) }

// HeaderFor is the first line of a journal in a given payload version, which a
// test or a migration may need to write deliberately.
func HeaderFor(version string) []byte {
	return []byte(magic + " " + ContainerVersion + " " + version + "\n")
}

// formatMetadata is the exact byte construction the checksum covers, and the
// exact bytes that appear on the header line between "BATCH " and " CRC32C:".
// One definition, so the two can never drift.
func formatMetadata(number, length, first, last, count uint64) string {
	return fmt.Sprintf("%020d %020d %020d %020d %020d", number, length, first, last, count)
}

// ReadJournal reads a whole journal, refusing what it cannot prove and
// reporting how the file ended.
func ReadJournal(r io.Reader) (*Journal, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxJournalBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxJournalBytes {
		return nil, fmt.Errorf("%w: journal exceeds %d bytes", ErrTooLarge, MaxJournalBytes)
	}

	line, rest, ok := splitLine(raw)
	if !ok {
		return nil, fmt.Errorf("%w: no version line", ErrMagic)
	}
	container, payloadVersion, err := parseVersionLine(line)
	if err != nil {
		return nil, err
	}

	j := &Journal{ContainerVersion: container, PayloadVersion: payloadVersion}
	j.ConfirmedBytes = int64(len(raw) - len(rest))

	var (
		expectedNumber   uint64 = 1
		expectedSequence uint64 = 1
	)

	for len(rest) > 0 {
		batch, consumed, status, header, err := readBatch(rest, expectedNumber, expectedSequence, payloadVersion)
		if err != nil {
			return nil, err
		}
		if status != TailComplete {
			// Anything after a damaged batch cannot be shown to continue it.
			// Skipping it would produce a state with a hole, and continuity is
			// the one property the numbering and sequence ranges exist to prove.
			if status == TailCorrupt && consumed < len(rest) {
				return nil, fmt.Errorf("%w: batch %d", ErrCorruptMidFile, expectedNumber)
			}
			j.Tail, j.DiscardedBytes = status, int64(len(rest))
			if status == TailCorrupt {
				j.TailBatch = header
			}
			return j, nil
		}
		j.ConfirmedBytes += int64(consumed)
		batch.EndOffset = j.ConfirmedBytes
		j.Batches = append(j.Batches, batch)
		expectedNumber++
		expectedSequence = batch.LastSequence + 1
		rest = rest[consumed:]
	}
	return j, nil
}

// readBatch reads one frame. It reports a tail status rather than an error for
// the two endings that are recoverable, and an error for the ones that are not.
func readBatch(raw []byte, wantNumber, wantSequence uint64, version string) (Batch, int, TailStatus, *TailBatch, error) {
	line, rest, ok := splitLine(raw)
	if !ok {
		return Batch{}, 0, TailIncomplete, nil, nil
	}
	number, length, first, last, count, sum, err := parseBatchHeader(line)
	if err != nil {
		return Batch{}, 0, TailComplete, nil, err
	}
	if length > MaxPayloadBytes {
		return Batch{}, 0, TailComplete, nil, fmt.Errorf("%w: batch %d claims %d bytes", ErrTooLarge, number, length)
	}
	if uint64(len(rest)) < length {
		return Batch{}, 0, TailIncomplete, nil, nil
	}
	payload := rest[:length]

	metadata := formatMetadata(number, length, first, last, count)
	if crc32.Checksum(append([]byte(metadata+"\n"), payload...), castagnoli) != sum {
		return Batch{}, len(line) + 1 + int(length), TailCorrupt,
			&TailBatch{Number: number, Length: length, FirstSequence: first, LastSequence: last, EventCount: count}, nil
	}

	if number != wantNumber {
		return Batch{}, 0, TailComplete, nil, fmt.Errorf("%w: batch numbered %d, want %d", ErrDiscontinuous, number, wantNumber)
	}
	if first != wantSequence {
		return Batch{}, 0, TailComplete, nil, fmt.Errorf("%w: batch %d starts at %d, want %d", ErrDiscontinuous, number, first, wantSequence)
	}

	events, err := DecodeEvents(payload, version)
	if err != nil {
		return Batch{}, 0, TailComplete, nil, err
	}
	if len(events) == 0 {
		return Batch{}, 0, TailComplete, nil, fmt.Errorf("%w: batch %d", ErrEmptyBatch, number)
	}
	// A matching checksum over disagreeing numbers means the file was written
	// wrong rather than damaged, which is the worse problem of the two.
	if uint64(len(events)) != count {
		return Batch{}, 0, TailComplete, nil, fmt.Errorf("%w: batch %d claims %d events, carries %d", ErrMetadataMismatch, number, count, len(events))
	}
	if events[0].Header().Sequence != first || events[len(events)-1].Header().Sequence != last {
		return Batch{}, 0, TailComplete, nil, fmt.Errorf("%w: batch %d claims sequences %d..%d, carries %d..%d",
			ErrMetadataMismatch, number, first, last, events[0].Header().Sequence, events[len(events)-1].Header().Sequence)
	}

	return Batch{Number: number, FirstSequence: first, LastSequence: last, Events: events},
		len(line) + 1 + int(length), TailComplete, nil, nil
}

func parseVersionLine(line string) (container, payload string, err error) {
	parts := strings.Split(line, " ")
	if len(parts) != 3 || parts[0] != magic {
		return "", "", fmt.Errorf("%w: %q", ErrMagic, line)
	}
	container, payload = parts[1], parts[2]
	if !compatible[container][payload] {
		return "", "", fmt.Errorf("%w: container %q with payload %q", ErrVersionPair, container, payload)
	}
	return container, payload, nil
}

func parseBatchHeader(line string) (number, length, first, last, count uint64, sum uint32, err error) {
	parts := strings.Split(line, " ")
	if len(parts) != 7 || parts[0] != batchKeyword {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("%w: %q", ErrFrame, line)
	}
	values := make([]uint64, 5)
	for n := range values {
		field := parts[n+1]
		if len(field) != 20 {
			return 0, 0, 0, 0, 0, 0, fmt.Errorf("%w: field %d is %d digits, want 20", ErrFrame, n, len(field))
		}
		v, perr := parseFixedWidth(field)
		if perr != nil {
			return 0, 0, 0, 0, 0, 0, perr
		}
		values[n] = v
	}
	crcField := parts[6]
	if !strings.HasPrefix(crcField, crcPrefix) || len(crcField) != len(crcPrefix)+8 {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("%w: checksum field %q", ErrFrame, crcField)
	}
	digits := crcField[len(crcPrefix):]
	var parsed uint64
	for _, c := range digits {
		switch {
		case c >= '0' && c <= '9':
			parsed = parsed*16 + uint64(c-'0')
		case c >= 'a' && c <= 'f':
			parsed = parsed*16 + uint64(c-'a'+10)
		default:
			// Uppercase hexadecimal is refused rather than accepted, so one
			// batch has exactly one valid encoding.
			return 0, 0, 0, 0, 0, 0, fmt.Errorf("%w: checksum %q is not eight lowercase hex digits", ErrFrame, digits)
		}
	}
	return values[0], values[1], values[2], values[3], values[4], uint32(parsed), nil
}

// parseFixedWidth reads a zero-padded header field. The header's rule is the
// opposite of the payload's on purpose: here every field is exactly 20 digits.
func parseFixedWidth(field string) (uint64, error) {
	var v uint64
	for _, c := range field {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: %q is not a number", ErrFrame, field)
		}
		v = v*10 + uint64(c-'0')
	}
	return v, nil
}

func splitLine(raw []byte) (line string, rest []byte, ok bool) {
	for n, c := range raw {
		if c == '\n' {
			return string(raw[:n]), raw[n+1:], true
		}
	}
	return "", nil, false
}

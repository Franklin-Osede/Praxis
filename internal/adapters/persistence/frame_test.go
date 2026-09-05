package persistence_test

import (
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"reflect"
	"strings"
	"testing"

	"praxis/internal/adapters/persistence"
	"praxis/internal/session"
)

func readJournal(t *testing.T, raw []byte) *persistence.Journal {
	t.Helper()
	j, err := persistence.ReadJournal(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadJournal: %v", err)
	}
	return j
}

// Scenario: a framed journal reads back as the batches it was written from
func TestAFramedJournalReadsBack(t *testing.T) {
	events := everyEventType()
	raw := journalBytes(t, events[:4], events[4:])

	j := readJournal(t, raw)

	if j.ContainerVersion != "1" || j.PayloadVersion != persistence.EventVersion {
		t.Fatalf("versions: got %q %q", j.ContainerVersion, j.PayloadVersion)
	}
	if j.Tail != persistence.TailComplete || j.DiscardedBytes != 0 {
		t.Fatalf("tail: got %v, %d bytes discarded", j.Tail, j.DiscardedBytes)
	}
	if len(j.Batches) != 2 {
		t.Fatalf("batches: got %d, want 2", len(j.Batches))
	}
	if j.Batches[0].Number != 1 || j.Batches[1].Number != 2 {
		t.Fatalf("numbers: got %d and %d", j.Batches[0].Number, j.Batches[1].Number)
	}
	if j.Batches[0].FirstSequence != 1 || int(j.Batches[1].LastSequence) != len(events) {
		t.Fatalf("sequences: got %+v", j.Batches)
	}
	if !reflect.DeepEqual(j.Events(), events) {
		t.Fatal("the recovered events differ from the ones written")
	}
}

// Scenario: the frame is a contract, byte for byte
func TestTheFrameGolden(t *testing.T) {
	raw := journalBytes(t, everyEventType()[:1])
	lines := strings.SplitN(string(raw), "\n", 3)

	if lines[0] != "PRAXIS-EVENT-STORE 1 "+persistence.EventVersion {
		t.Fatalf("version line: got %q", lines[0])
	}

	payload := "session_started 1000 1 MNQ 50 5000000 50 5000000 100000 300000 200000 250000\n"
	metadata := fmt.Sprintf("%020d %020d %020d %020d %020d", 1, len(payload), 1, 1, 1)
	sum := crc32.Checksum([]byte(metadata+"\n"+payload), crc32.MakeTable(crc32.Castagnoli))
	want := fmt.Sprintf("BATCH %s CRC32C:%08x", metadata, sum)

	if lines[1] != want {
		t.Fatalf("header line\n got: %q\nwant: %q", lines[1], want)
	}
	if lines[2] != payload {
		t.Fatalf("payload: got %q", lines[2])
	}

	// The checksum is Castagnoli over the metadata and the payload together,
	// so a header field cannot be changed while the checksum still matches.
	if strings.Count(lines[1], " ") != 6 {
		t.Fatalf("header separators: got %d spaces, want exactly 6", strings.Count(lines[1], " "))
	}
	for _, field := range strings.Split(lines[1], " ")[1:6] {
		if len(field) != 20 {
			t.Fatalf("field %q is %d digits, want 20", field, len(field))
		}
	}
}

// Scenario: encoding a batch twice produces the same bytes
func TestEncodingABatchTwiceIsIdentical(t *testing.T) {
	first, err := persistence.EncodeBatch(7, everyEventType(), persistence.EventVersion)
	if err != nil {
		t.Fatalf("EncodeBatch: %v", err)
	}
	again, err := persistence.EncodeBatch(7, everyEventType(), persistence.EventVersion)
	if err != nil {
		t.Fatalf("EncodeBatch: %v", err)
	}
	if !bytes.Equal(first, again) {
		t.Fatal("the same batch encoded to different bytes")
	}
}

// Scenario: altering any header field is caught, because the checksum covers
// the metadata and not only the payload
func TestAlteringAnyHeaderFieldIsCaught(t *testing.T) {
	fields := []struct {
		name  string
		index int
	}{
		{"the batch number", 1},
		{"the payload length", 2},
		{"the first sequence", 3},
		{"the last sequence", 4},
		{"the event count", 5},
	}

	for _, f := range fields {
		t.Run(f.name, func(t *testing.T) {
			raw := journalBytes(t, everyEventType())
			lines := strings.SplitN(string(raw), "\n", 3)
			parts := strings.Split(lines[1], " ")

			var v uint64
			fmt.Sscanf(parts[f.index], "%d", &v)
			parts[f.index] = fmt.Sprintf("%020d", v+1)
			altered := lines[0] + "\n" + strings.Join(parts, " ") + "\n" + lines[2]

			j, err := persistence.ReadJournal(strings.NewReader(altered))
			if err != nil {
				return // refused outright, which is also correct
			}
			if j.Tail == persistence.TailComplete {
				t.Fatalf("altering %s was accepted as a complete journal", f.name)
			}
		})
	}
}

// Scenario: altering one byte of the payload is caught
func TestAlteringOnePayloadByteIsCaught(t *testing.T) {
	raw := journalBytes(t, everyEventType())
	at := bytes.Index(raw, []byte("session_started"))
	if at < 0 {
		t.Fatal("the payload was not found")
	}
	raw[at+len("session_started ")] = '9'

	j, err := persistence.ReadJournal(bytes.NewReader(raw))
	if err != nil {
		return
	}
	if j.Tail != persistence.TailCorrupt {
		t.Fatalf("tail: got %v, want corrupt", j.Tail)
	}
	if len(j.Batches) != 0 {
		t.Fatalf("batches: got %d, want the damaged batch withheld", len(j.Batches))
	}
}

// Scenario: a journal cut at any offset ends as an unconfirmed tail, never as
// a silently shorter but valid journal
//
// This is the ordinary shape of a process killed mid-append. Every prefix of
// the last batch must be discarded whole, and the batches before it must
// survive untouched.
func TestTruncationAtEveryOffsetOfTheLastBatch(t *testing.T) {
	events := everyEventType()
	full := journalBytes(t, events[:4], events[4:])
	firstBatchEnd := len(journalBytes(t, events[:4]))

	for cut := firstBatchEnd; cut < len(full); cut++ {
		j, err := persistence.ReadJournal(bytes.NewReader(full[:cut]))
		if err != nil {
			t.Fatalf("cut at %d: %v", cut, err)
		}
		if len(j.Batches) != 1 {
			t.Fatalf("cut at %d: got %d batches, want only the confirmed one", cut, len(j.Batches))
		}
		if !reflect.DeepEqual(j.Events(), events[:4]) {
			t.Fatalf("cut at %d: the confirmed batch changed", cut)
		}
		if cut == firstBatchEnd {
			if j.Tail != persistence.TailComplete {
				t.Fatalf("cut exactly on the boundary: got %v, want complete", j.Tail)
			}
			continue
		}
		if j.Tail != persistence.TailIncomplete {
			t.Fatalf("cut at %d: got %v, want incomplete", cut, j.Tail)
		}
		if j.DiscardedBytes != int64(cut-firstBatchEnd) {
			t.Fatalf("cut at %d: discarded %d bytes, want %d", cut, j.DiscardedBytes, cut-firstBatchEnd)
		}
	}
}

// Scenario: a damaged batch with data after it is fatal
//
// Skipping it would produce a state with a hole in it, and continuity is the
// one property the numbering and the sequence ranges exist to prove.
func TestADamagedBatchFollowedByDataIsFatal(t *testing.T) {
	events := everyEventType()
	raw := journalBytes(t, events[:4], events[4:])

	at := bytes.Index(raw, []byte("session_started"))
	raw[at+len("session_started ")] = '9'

	if _, err := persistence.ReadJournal(bytes.NewReader(raw)); !errors.Is(err, persistence.ErrCorruptMidFile) {
		t.Fatalf("error: got %v, want %v", err, persistence.ErrCorruptMidFile)
	}
}

// Scenario: metadata that disagrees with its payload is an error even when the
// checksum matches, because that means the file was written wrong rather than
// damaged
func TestMetadataDisagreeingWithItsPayloadIsRefused(t *testing.T) {
	events := everyEventType()
	payload, err := persistence.EncodeEvents(events, persistence.EventVersion)
	if err != nil {
		t.Fatalf("EncodeEvents: %v", err)
	}

	tests := []struct {
		name                             string
		number, length, first, last, cnt uint64
		want                             error
	}{
		{"an event count that lies", 1, uint64(len(payload)), 1, 9, 8, persistence.ErrMetadataMismatch},
		{"a last sequence that lies", 1, uint64(len(payload)), 1, 8, 9, persistence.ErrMetadataMismatch},
		{"a batch number out of order", 2, uint64(len(payload)), 1, 9, 9, persistence.ErrDiscontinuous},
		{"a first sequence out of order", 1, uint64(len(payload)), 2, 9, 9, persistence.ErrDiscontinuous},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			metadata := fmt.Sprintf("%020d %020d %020d %020d %020d", tc.number, tc.length, tc.first, tc.last, tc.cnt)
			sum := crc32.Checksum(append([]byte(metadata+"\n"), payload...), crc32.MakeTable(crc32.Castagnoli))
			raw := string(persistence.Header()) +
				fmt.Sprintf("BATCH %s CRC32C:%08x\n", metadata, sum) + string(payload)

			if _, err := persistence.ReadJournal(strings.NewReader(raw)); !errors.Is(err, tc.want) {
				t.Fatalf("error: got %v, want %v", err, tc.want)
			}
		})
	}
}

// Scenario: only known combinations of container and payload version are read
func TestVersionCombinationsAreExplicit(t *testing.T) {
	tests := []struct {
		name string
		line string
		want error
	}{
		{"a known container with an unknown payload", "PRAXIS-EVENT-STORE 1 praxis.event.v9\n", persistence.ErrVersionPair},
		{"an unknown container with a known payload", "PRAXIS-EVENT-STORE 2 praxis.event.v1\n", persistence.ErrVersionPair},
		{"another file entirely", "SOMETHING-ELSE 1 praxis.event.v1\n", persistence.ErrMagic},
		{"a version line with a missing field", "PRAXIS-EVENT-STORE 1\n", persistence.ErrMagic},
		{"an empty file", "", persistence.ErrMagic},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := persistence.ReadJournal(strings.NewReader(tc.line)); !errors.Is(err, tc.want) {
				t.Fatalf("error: got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestRefusesAMalformedHeaderOrAnOversizedBatch(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   error
	}{
		{"a short field", "BATCH 1 2 3 4 5 CRC32C:00000000\n", persistence.ErrFrame},
		{"uppercase hexadecimal", fmt.Sprintf("BATCH %020d %020d %020d %020d %020d CRC32C:ABCDEF01\n", 1, 1, 1, 1, 1), persistence.ErrFrame},
		{"a missing checksum", fmt.Sprintf("BATCH %020d %020d %020d %020d %020d\n", 1, 1, 1, 1, 1), persistence.ErrFrame},
		{"a length beyond the format's limit", fmt.Sprintf("BATCH %020d %020d %020d %020d %020d CRC32C:00000000\n", 1, persistence.MaxPayloadBytes+1, 1, 1, 1), persistence.ErrTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := string(persistence.Header()) + tc.header
			if _, err := persistence.ReadJournal(strings.NewReader(raw)); !errors.Is(err, tc.want) {
				t.Fatalf("error: got %v, want %v", err, tc.want)
			}
		})
	}
}

// An empty batch cannot be written: a command that produced no events produced
// no batch.
func TestAnEmptyBatchCannotBeWritten(t *testing.T) {
	if _, err := persistence.EncodeBatch(1, nil, persistence.EventVersion); !errors.Is(err, persistence.ErrEmptyBatch) {
		t.Fatalf("error: got %v, want %v", err, persistence.ErrEmptyBatch)
	}
}

// Property: a real session's journal survives framing, and what comes back
// still replays.
func TestPropertyARealJournalSurvivesFraming(t *testing.T) {
	events := realSessionEvents(t)
	raw := journalBytes(t, events)

	j := readJournal(t, raw)
	if !reflect.DeepEqual(j.Events(), events) {
		t.Fatal("a real journal did not survive framing")
	}
	if _, err := session.Replay(j.Events()); err != nil {
		t.Fatalf("Replay of the recovered journal: %v", err)
	}
}

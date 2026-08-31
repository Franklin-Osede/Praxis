package persistence_test

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"praxis/internal/adapters/persistence"
	"praxis/internal/market"
	"praxis/internal/session"
)

func golden(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/golden-events.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return b
}

// Scenario: encoding and decoding preserve the events exactly
func TestDecodeOfEncodeIsTheIdentity(t *testing.T) {
	events := everyEventType()

	payload, err := persistence.EncodeEvents(events)
	if err != nil {
		t.Fatalf("EncodeEvents: %v", err)
	}
	got, err := persistence.DecodeEvents(payload)
	if err != nil {
		t.Fatalf("DecodeEvents: %v", err)
	}
	if !reflect.DeepEqual(got, events) {
		t.Fatalf("round trip\n got: %+v\nwant: %+v", got, events)
	}
}

// Scenario: the encoding is canonical
//
//	Given a canonical payload
//	When it is decoded and encoded again
//	Then the bytes are identical. A decoder that normalised an alternative
//	  spelling would let two different files mean the same thing, and this
//	  is what makes replay equality a property of the domain rather than of
//	  an encoder.
func TestEncodeOfDecodeReproducesTheBytes(t *testing.T) {
	canonical := golden(t)

	events, err := persistence.DecodeEvents(canonical)
	if err != nil {
		t.Fatalf("DecodeEvents: %v", err)
	}
	got, err := persistence.EncodeEvents(events)
	if err != nil {
		t.Fatalf("EncodeEvents: %v", err)
	}
	if string(got) != string(canonical) {
		t.Fatalf("re-encoded bytes differ\n got: %q\nwant: %q", got, canonical)
	}
}

// Scenario: the format is a contract, not whatever the code happens to emit
//
// Encoder and decoder can be changed together and keep every property while
// silently breaking every journal written before. Literal expected bytes are
// what stops that, so praxis.event.v1 means one thing forever.
func TestTheGoldenBytesAreTheFormat(t *testing.T) {
	payload, err := persistence.EncodeEvents(everyEventType())
	if err != nil {
		t.Fatalf("EncodeEvents: %v", err)
	}
	if string(payload) != string(golden(t)) {
		t.Fatalf("the format changed\n got:\n%s\nwant:\n%s", payload, golden(t))
	}

	lines := strings.Split(strings.TrimSuffix(string(payload), "\n"), "\n")
	if len(lines) != 9 {
		t.Fatalf("lines: got %d, want one per event type", len(lines))
	}
	if strings.Contains(string(payload), "\r") {
		t.Fatal("the payload contains a carriage return")
	}
	if !strings.HasSuffix(string(payload), "\n") {
		t.Fatal("the payload does not end with a newline")
	}
}

// Scenario: an alternative spelling of a canonical value is refused, never
// normalised
func TestDecodeRefusesNonCanonicalText(t *testing.T) {
	const valid = "session_opened 2000 2 d1 5000000 5000000\n"

	tests := []struct {
		name    string
		payload string
		want    error
	}{
		{"a leading plus", "session_opened 2000 2 d1 +5000000 5000000\n", persistence.ErrNotCanonicalInt},
		{"a leading zero", "session_opened 2000 2 d1 05000000 5000000\n", persistence.ErrNotCanonicalInt},
		{"a negative zero", "session_opened 2000 2 d1 -0 5000000\n", persistence.ErrNotCanonicalInt},
		{"a decimal point", "session_opened 2000 2 d1 5000000.0 5000000\n", persistence.ErrNotCanonicalInt},
		{"two spaces", "session_opened 2000 2 d1  5000000 5000000\n", persistence.ErrSyntax},
		{"a trailing space", "session_opened 2000 2 d1 5000000 5000000 \n", persistence.ErrSyntax},
		{"a carriage return", "session_opened 2000 2 d1 5000000 5000000\r\n", persistence.ErrSyntax},
		{"an extra field", "session_opened 2000 2 d1 5000000 5000000 7\n", persistence.ErrSyntax},
		{"a missing field", "session_opened 2000 2 d1 5000000\n", persistence.ErrSyntax},
		{"an unknown event type", "session_paused 2000 2 d1\n", persistence.ErrUnknownEvent},
		{"a numeric enumeration", "fill_produced 3000 7 o-1 MNQ 50 3000 2 20050 3\n", persistence.ErrSyntax},
		{"an unknown enumeration", "fill_produced 3000 7 o-1 MNQ 50 3000 short 20050 3\n", persistence.ErrSyntax},
		{"an identifier outside the alphabet", "session_opened 2000 2 d@1 5000000 5000000\n", persistence.ErrIdentifier},
		{"an empty identifier", "session_ended 9000 9 \n", persistence.ErrSyntax},
		{"no final newline", strings.TrimSuffix(valid, "\n"), persistence.ErrTrailingBytes},
		{"a blank line", valid + "\n", persistence.ErrSyntax},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := persistence.DecodeEvents([]byte(tc.payload)); !errors.Is(err, tc.want) {
				t.Fatalf("error: got %v, want %v", err, tc.want)
			}
		})
	}

	if _, err := persistence.DecodeEvents([]byte(valid)); err != nil {
		t.Fatalf("the valid line does not decode: %v", err)
	}
}

// Scenario: the encoder refuses what it could not read back
func TestEncodeRefusesWhatItCouldNotReadBack(t *testing.T) {
	tests := []struct {
		name  string
		event session.Event
		want  error
	}{
		{
			name: "an identifier outside the alphabet",
			event: session.SessionEnded{
				Envelope:  session.Envelope{Time: 1, Sequence: 1, Kind: session.KindSessionEnded},
				SessionID: "d 1",
			},
			want: persistence.ErrIdentifier,
		},
		{
			name: "an event whose kind contradicts its type",
			event: session.SessionEnded{
				Envelope:  session.Envelope{Time: 1, Sequence: 1, Kind: session.KindMarketObserved},
				SessionID: "d1",
			},
			want: persistence.ErrKindMismatch,
		},
		{
			name: "an enumeration with no name",
			event: session.FillProduced{
				Envelope: session.Envelope{Time: 1, Sequence: 1, Kind: session.KindFillProduced},
				Fill:     market.Fill{OrderID: "o-1", Instrument: mnq, Side: market.SideUnspecified, Price: 1, Qty: 1},
			},
			want: persistence.ErrSyntax,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := persistence.EncodeEvents([]session.Event{tc.event}); !errors.Is(err, tc.want) {
				t.Fatalf("error: got %v, want %v", err, tc.want)
			}
		})
	}
}

// A payload larger than the format allows is refused before memory is
// reserved for it.
func TestRefusesInputBeyondTheFormatsLimits(t *testing.T) {
	oversized := make([]byte, persistence.MaxPayloadBytes+1)
	for n := range oversized {
		oversized[n] = 'a'
	}
	if _, err := persistence.DecodeEvents(oversized); !errors.Is(err, persistence.ErrTooLarge) {
		t.Fatalf("error: got %v, want %v", err, persistence.ErrTooLarge)
	}

	long := session.SessionEnded{
		Envelope:  session.Envelope{Time: 1, Sequence: 1, Kind: session.KindSessionEnded},
		SessionID: sessionIDOf(persistence.MaxLineBytes + 1),
	}
	if _, err := persistence.EncodeEvents([]session.Event{long}); !errors.Is(err, persistence.ErrTooLarge) {
		t.Fatalf("error: got %v, want %v", err, persistence.ErrTooLarge)
	}
}

// Property: a whole real session encodes and decodes to itself.
func TestPropertyARealJournalRoundTrips(t *testing.T) {
	events := realSessionEvents(t)

	payload, err := persistence.EncodeEvents(events)
	if err != nil {
		t.Fatalf("EncodeEvents: %v", err)
	}
	got, err := persistence.DecodeEvents(payload)
	if err != nil {
		t.Fatalf("DecodeEvents: %v", err)
	}
	if !reflect.DeepEqual(got, events) {
		t.Fatal("a real journal did not survive the round trip")
	}

	// And what came back still replays, so the codec preserved everything
	// reconstruction depends on.
	if _, err := session.Replay(got); err != nil {
		t.Fatalf("Replay of the decoded journal: %v", err)
	}

	again, err := persistence.EncodeEvents(got)
	if err != nil {
		t.Fatalf("EncodeEvents: %v", err)
	}
	if string(again) != string(payload) {
		t.Fatal("re-encoding a real journal produced different bytes")
	}
}

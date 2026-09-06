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

func golden(t *testing.T) []byte { return goldenFor(t, "testdata/golden-events.txt") }

func goldenFor(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return b
}

// Scenario: encoding and decoding preserve the events exactly
func TestDecodeOfEncodeIsTheIdentity(t *testing.T) {
	for _, tc := range []struct {
		version string
		events  []session.Event
	}{
		{persistence.EventVersionV1, everyEventType()},
		{persistence.EventVersionV2, everyEventTypeV2()},
		{persistence.EventVersionV3, everyEventTypeV3()},
		{persistence.EventVersionV4, everyEventTypeV4()},
	} {
		t.Run(tc.version, func(t *testing.T) {
			payload, err := persistence.EncodeEvents(tc.events, tc.version)
			if err != nil {
				t.Fatalf("EncodeEvents: %v", err)
			}
			got, err := persistence.DecodeEvents(payload, tc.version)
			if err != nil {
				t.Fatalf("DecodeEvents: %v", err)
			}
			if !reflect.DeepEqual(got, tc.events) {
				t.Fatalf("round trip\n got: %+v\nwant: %+v", got, tc.events)
			}
		})
	}
}

// Scenario: a version cannot express what it does not have
//
//	Given a fact that only the newer schema carries
//	When it is written in the older one
//	Then it is refused rather than dropped. A v1 journal honestly lacks
//	  protection; it must never appear to hold it, and must never silently
//	  lose it either.
func TestAVersionRefusesWhatItCannotExpress(t *testing.T) {
	protection := session.ProtectionPlaced{
		Envelope:     session.Envelope{Time: 1, Sequence: 1, Kind: session.KindProtectionPlaced},
		EntryOrderID: "o-1", StopPrice: 19_900,
	}
	for _, older := range []string{persistence.EventVersionV1, persistence.EventVersionV2} {
		if _, err := persistence.EncodeEvents([]session.Event{protection}, older); !errors.Is(err, persistence.ErrUnsupportedInVersion) {
			t.Fatalf("protection under %s: got %v, want %v", older, err, persistence.ErrUnsupportedInVersion)
		}
	}
	if _, err := persistence.EncodeEvents([]session.Event{protection}, persistence.EventVersionV3); err != nil {
		t.Fatalf("protection under v3: %v", err)
	}

	// A reason is a value, not a field, and an older version has no name for
	// it. Writing it under v3 would produce a line a v3 reader must reject, so
	// the writer refuses first.
	byOCO := session.OrderCancelled{
		Envelope: session.Envelope{Time: 1, Sequence: 1, Kind: session.KindOrderCancelled},
		OrderID:  "praxis:1:target", RemainingQty: 3, Reason: session.CancelledByOCO,
	}
	for _, older := range []string{persistence.EventVersionV1, persistence.EventVersionV2, persistence.EventVersionV3} {
		if _, err := persistence.EncodeEvents([]session.Event{byOCO}, older); !errors.Is(err, persistence.ErrUnsupportedInVersion) {
			t.Fatalf("by_oco under %s: got %v, want %v", older, err, persistence.ErrUnsupportedInVersion)
		}
	}
	if _, err := persistence.EncodeEvents([]session.Event{byOCO}, persistence.EventVersionV4); err != nil {
		t.Fatalf("by_oco under v4: %v", err)
	}

	// Who traded a journal is a field, and a field is the other kind of thing
	// an older reader cannot parse. It is refused rather than dropped, because
	// a journal that silently lost its subject would be a session belonging to
	// nobody — and the experiment's unit of analysis is a person.
	subject := session.SessionStarted{
		Envelope: session.Envelope{Time: 1, Sequence: 1, Kind: session.KindSessionStarted},
		Config: session.Config{
			Instrument: mnq, StartingBalanceCts: 1, CommissionPerContractCts: 0,
			SubjectID: "s-07",
		},
	}
	for _, older := range []string{persistence.EventVersionV1, persistence.EventVersionV2, persistence.EventVersionV3} {
		if _, err := persistence.EncodeEvents([]session.Event{subject}, older); !errors.Is(err, persistence.ErrUnsupportedInVersion) {
			t.Fatalf("a subject under %s: got %v, want %v", older, err, persistence.ErrUnsupportedInVersion)
		}
	}
	if _, err := persistence.EncodeEvents([]session.Event{subject}, persistence.EventVersionV4); err != nil {
		t.Fatalf("a subject under v4: %v", err)
	}
	// And a journal with no subject still writes under v4, because a scripted
	// run belongs to nobody and saying so is not the same as losing it.
	subject.Config.SubjectID = ""
	if _, err := persistence.EncodeEvents([]session.Event{subject}, persistence.EventVersionV1); err != nil {
		t.Fatalf("no subject under v1: %v", err)
	}

	// When a person acted is the other v4 field, and it is refused the same
	// way. A behavioural record that silently lost when its decisions were
	// taken would be missing the behaviour.
	acted := session.OrderSubmitted{
		Envelope:  session.Envelope{Time: 1, Sequence: 1, Kind: session.KindOrderSubmitted},
		Order:     market.Order{ID: "o-1", Instrument: mnq, Side: market.SideBuy, Type: market.OrderTypeMarket, Qty: 1},
		DecidedAt: humanAt,
	}
	for _, older := range []string{persistence.EventVersionV2, persistence.EventVersionV3} {
		if _, err := persistence.EncodeEvents([]session.Event{acted}, older); !errors.Is(err, persistence.ErrUnsupportedInVersion) {
			t.Fatalf("a human clock under %s: got %v, want %v", older, err, persistence.ErrUnsupportedInVersion)
		}
	}
	if _, err := persistence.EncodeEvents([]session.Event{acted}, persistence.EventVersionV4); err != nil {
		t.Fatalf("a human clock under v4: %v", err)
	}
	// And an older reader refuses the name rather than guessing at it, even
	// though the rest of the line is one it understands perfectly.
	v4Line, err := persistence.EncodeEvents([]session.Event{byOCO}, persistence.EventVersionV4)
	if err != nil {
		t.Fatalf("EncodeEvents: %v", err)
	}
	if _, err := persistence.DecodeEvents(v4Line, persistence.EventVersionV3); !errors.Is(err, persistence.ErrUnsupportedInVersion) {
		t.Fatalf("v3 read a v4 reason: got %v, want %v", err, persistence.ErrUnsupportedInVersion)
	}

	// A streak that v1 has no field for is refused rather than quietly lost.
	var withStreak []session.Event
	for _, e := range everyEventTypeV3() {
		if o, ok := e.(session.OrderSubmitted); ok {
			withStreak = append(withStreak, o)
		}
	}
	if len(withStreak) == 0 {
		t.Fatal("the fixture carries no decision")
	}
	if _, err := persistence.EncodeEvents(withStreak, persistence.EventVersionV1); !errors.Is(err, persistence.ErrUnsupportedInVersion) {
		t.Fatalf("a streak under v1: got %v, want %v", err, persistence.ErrUnsupportedInVersion)
	}

	// Reading a payload as an OLDER version is refused rather than guessed at,
	// in every direction. That is the property, and every ordered pair is
	// tried rather than a chosen few, because a table that listed some of them
	// would pin whichever ones happened to pass.
	//
	// The reverse does not hold and is not claimed. A version that only adds
	// event types or values leaves an older payload a valid subset of itself,
	// so v2 bytes read as v3 decode perfectly; a version that adds a field to
	// an existing line does not, so v1 bytes read as v2 are refused. Which of
	// those a given step was is a fact about that step, not a rule. Nothing
	// inside a payload says which version it is — the frame carries that, and
	// the frame is what a reader must believe.
	rank := map[string]int{
		persistence.EventVersionV1: 1, persistence.EventVersionV2: 2,
		persistence.EventVersionV3: 3, persistence.EventVersionV4: 4,
	}
	goldens := map[string]string{
		persistence.EventVersionV1: "testdata/golden-events.txt",
		persistence.EventVersionV2: "testdata/golden-events-v2.txt",
		persistence.EventVersionV3: "testdata/golden-events-v3.txt",
		persistence.EventVersionV4: "testdata/golden-events-v4.txt",
	}
	versions := []string{
		persistence.EventVersionV1, persistence.EventVersionV2,
		persistence.EventVersionV3, persistence.EventVersionV4,
	}
	for _, wrote := range versions {
		for _, readAs := range versions {
			if rank[readAs] >= rank[wrote] {
				continue
			}
			if _, err := persistence.DecodeEvents(goldenFor(t, goldens[wrote]), readAs); err == nil {
				t.Fatalf("%s bytes were read as the older %s", wrote, readAs)
			}
		}
	}

	// And the asymmetry is written down rather than left to be discovered: a
	// payload mislabelled as a later version can decode silently and
	// completely, which is why the frame's version is not advisory.
	if _, err := persistence.DecodeEvents(goldenFor(t, goldens[persistence.EventVersionV2]), persistence.EventVersionV3); err != nil {
		t.Fatalf("v2 bytes no longer read as v3, so the asymmetry this documents has changed: %v", err)
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
	for _, tc := range []struct{ version, path string }{
		{persistence.EventVersionV1, "testdata/golden-events.txt"},
		{persistence.EventVersionV2, "testdata/golden-events-v2.txt"},
		{persistence.EventVersionV3, "testdata/golden-events-v3.txt"},
		{persistence.EventVersionV4, "testdata/golden-events-v4.txt"},
	} {
		t.Run(tc.version, func(t *testing.T) {
			canonical := goldenFor(t, tc.path)
			events, err := persistence.DecodeEvents(canonical, tc.version)
			if err != nil {
				t.Fatalf("DecodeEvents: %v", err)
			}
			got, err := persistence.EncodeEvents(events, tc.version)
			if err != nil {
				t.Fatalf("EncodeEvents: %v", err)
			}
			if string(got) != string(canonical) {
				t.Fatalf("re-encoded bytes differ\n got: %q\nwant: %q", got, canonical)
			}
		})
	}
}

// Scenario: the format is a contract, not whatever the code happens to emit
//
// Encoder and decoder can be changed together and keep every property while
// silently breaking every journal written before. Literal expected bytes are
// what stops that, so praxis.event.v1 means one thing forever.
func TestTheGoldenBytesAreTheFormat(t *testing.T) {
	for _, tc := range []struct {
		version string
		events  []session.Event
		path    string
		lines   int
	}{
		{persistence.EventVersionV1, everyEventType(), "testdata/golden-events.txt", 11},
		{persistence.EventVersionV2, everyEventTypeV2(), "testdata/golden-events-v2.txt", 11},
		{persistence.EventVersionV3, everyEventTypeV3(), "testdata/golden-events-v3.txt", 14},
		{persistence.EventVersionV4, everyEventTypeV4(), "testdata/golden-events-v4.txt", 16},
	} {
		t.Run(tc.version, func(t *testing.T) {
			payload, err := persistence.EncodeEvents(tc.events, tc.version)
			if err != nil {
				t.Fatalf("EncodeEvents: %v", err)
			}
			want := goldenFor(t, tc.path)
			if string(payload) != string(want) {
				t.Fatalf("the format changed\n got:\n%s\nwant:\n%s", payload, want)
			}
			lines := strings.Split(strings.TrimSuffix(string(payload), "\n"), "\n")
			if len(lines) != tc.lines {
				t.Fatalf("lines: got %d, want %d", len(lines), tc.lines)
			}
		})
	}

	payload := golden(t)
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
			if _, err := persistence.DecodeEvents([]byte(tc.payload), persistence.EventVersion); !errors.Is(err, tc.want) {
				t.Fatalf("error: got %v, want %v", err, tc.want)
			}
		})
	}

	if _, err := persistence.DecodeEvents([]byte(valid), persistence.EventVersion); err != nil {
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
			if _, err := persistence.EncodeEvents([]session.Event{tc.event}, persistence.EventVersion); !errors.Is(err, tc.want) {
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
	if _, err := persistence.DecodeEvents(oversized, persistence.EventVersion); !errors.Is(err, persistence.ErrTooLarge) {
		t.Fatalf("error: got %v, want %v", err, persistence.ErrTooLarge)
	}

	long := session.SessionEnded{
		Envelope:  session.Envelope{Time: 1, Sequence: 1, Kind: session.KindSessionEnded},
		SessionID: sessionIDOf(persistence.MaxLineBytes + 1),
	}
	if _, err := persistence.EncodeEvents([]session.Event{long}, persistence.EventVersion); !errors.Is(err, persistence.ErrTooLarge) {
		t.Fatalf("error: got %v, want %v", err, persistence.ErrTooLarge)
	}
}

// Property: a whole real session encodes and decodes to itself.
func TestPropertyARealJournalRoundTrips(t *testing.T) {
	events := realSessionEvents(t)

	payload, err := persistence.EncodeEvents(events, persistence.EventVersion)
	if err != nil {
		t.Fatalf("EncodeEvents: %v", err)
	}
	got, err := persistence.DecodeEvents(payload, persistence.EventVersion)
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

	again, err := persistence.EncodeEvents(got, persistence.EventVersion)
	if err != nil {
		t.Fatalf("EncodeEvents: %v", err)
	}
	if string(again) != string(payload) {
		t.Fatal("re-encoding a real journal produced different bytes")
	}
}

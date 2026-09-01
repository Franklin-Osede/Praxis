package marketdata_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"praxis/internal/adapters/marketdata"
	"praxis/internal/challenge"
	"praxis/internal/market"
	"praxis/internal/session"
)

var mnq = market.Instrument{Symbol: "MNQ", CentsPerTick: 50}

const header = "praxis.market.v1,MNQ,50\ntime,sequence,session_id,bid,ask,bid_size,ask_size\n"

func read(t *testing.T, body string) (*marketdata.Feed, error) {
	t.Helper()
	return marketdata.Read(strings.NewReader(header + body))
}

// Scenario: a canonical file becomes ordered observations
//
//	Given a file with two trading sessions
//	When it is read
//	Then every row is an observation carrying the session it belongs to,
//	  in the order the file states.
func TestReadsACanonicalFile(t *testing.T) {
	feed, err := marketdata.ReadFile("testdata/two-sessions.csv")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if feed.Instrument != mnq {
		t.Fatalf("instrument: got %+v, want %+v", feed.Instrument, mnq)
	}
	if len(feed.Observations) != 5 {
		t.Fatalf("observations: got %d, want 5", len(feed.Observations))
	}

	first := feed.Observations[0]
	want := marketdata.Observation{
		SessionID: "d1", Sequence: 1,
		Quote: market.Quote{
			Instrument: mnq, Time: 3_000,
			Bid: 20_000, Ask: 20_001, BidSize: 10, AskSize: 10,
		},
	}
	if first != want {
		t.Fatalf("first observation\n got: %+v\nwant: %+v", first, want)
	}

	// Two rows share a logical time and are told apart by their sequence.
	if feed.Observations[1].Quote.Time != 3_000 || feed.Observations[1].Sequence != 2 {
		t.Fatalf("second observation: got %+v", feed.Observations[1])
	}
	if feed.Observations[3].SessionID != "d2" {
		t.Fatalf("fourth observation: got session %v, want d2", feed.Observations[3].SessionID)
	}
}

// Scenario: a file that cannot be vouched for is refused
func TestReadRefusesAFileItCannotVouchFor(t *testing.T) {
	tests := []struct {
		name string
		file string
		want error
	}{
		{
			name: "an unknown version",
			file: "praxis.market.v2,MNQ,50\ntime,sequence,session_id,bid,ask,bid_size,ask_size\n3000,1,d1,20000,20001,10,10\n",
			want: marketdata.ErrVersion,
		},
		{
			name: "an instrument with no tick value",
			file: "praxis.market.v1,MNQ,0\ntime,sequence,session_id,bid,ask,bid_size,ask_size\n3000,1,d1,20000,20001,10,10\n",
			want: market.ErrNonPositiveTickValue,
		},
		{
			name: "an empty file",
			file: "",
			want: marketdata.ErrVersion,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := marketdata.Read(strings.NewReader(tc.file)); !errors.Is(err, tc.want) {
				t.Fatalf("error: got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReadRefusesABadHeader(t *testing.T) {
	tests := []struct {
		name   string
		header string
	}{
		{"a missing column", "time,sequence,session_id,bid,ask,bid_size\n"},
		{"an unknown column", "time,sequence,session_id,bid,ask,bid_size,ask_size,venue\n"},
		{"a renamed column", "time,sequence,session,bid,ask,bid_size,ask_size\n"},
		{"reordered columns", "sequence,time,session_id,bid,ask,bid_size,ask_size\n"},
		{"no header at all", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			file := "praxis.market.v1,MNQ,50\n" + tc.header
			if _, err := marketdata.Read(strings.NewReader(file)); !errors.Is(err, marketdata.ErrHeader) {
				t.Fatalf("error: got %v, want %v", err, marketdata.ErrHeader)
			}
		})
	}
}

// Scenario: a row the domain could not accept is refused where it is read
func TestReadRefusesABadRow(t *testing.T) {
	tests := []struct {
		name string
		body string
		want error
	}{
		{
			name: "rows out of order",
			body: "4000,1,d1,20000,20001,10,10\n3000,1,d1,20000,20001,10,10\n",
			want: marketdata.ErrOutOfOrder,
		},
		{
			name: "a repeated position",
			body: "3000,1,d1,20000,20001,10,10\n3000,1,d1,20000,20001,10,10\n",
			want: marketdata.ErrOutOfOrder,
		},
		{
			name: "a session that reappears",
			body: "3000,1,d1,20000,20001,10,10\n4000,1,d2,20000,20001,10,10\n5000,1,d1,20000,20001,10,10\n",
			want: marketdata.ErrSessionReturned,
		},
		{
			name: "a crossed book",
			body: "3000,1,d1,20002,20001,10,10\n",
			want: market.ErrCrossedQuote,
		},
		{
			name: "a negative size",
			body: "3000,1,d1,20000,20001,-1,10\n",
			want: market.ErrNegativeSize,
		},
		{
			name: "a non-positive price",
			body: "3000,1,d1,0,20001,10,10\n",
			want: market.ErrNonPositivePrice,
		},
		{
			name: "no session identifier",
			body: "3000,1,d1,20000,20001,10,10\n4000,1,,20000,20001,10,10\n",
			want: marketdata.ErrEmptySessionID,
		},
		{
			name: "a price too large to represent",
			body: "3000,1,d1,99999999999999999999,20001,10,10\n",
			want: marketdata.ErrField,
		},
		{
			name: "a negative sequence",
			body: "3000,-1,d1,20000,20001,10,10\n",
			want: marketdata.ErrField,
		},
		{
			name: "a row that is not a number",
			body: "later,1,d1,20000,20001,10,10\n",
			want: marketdata.ErrField,
		},
		{
			name: "a truncated final row",
			body: "3000,1,d1,20000,20001,10,10\n4000,1,d1,20010\n",
			want: marketdata.ErrTruncated,
		},
		{
			name: "a file with a header and nothing else",
			body: "",
			want: marketdata.ErrNoObservations,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := read(t, tc.body); !errors.Is(err, tc.want) {
				t.Fatalf("error: got %v, want %v", err, tc.want)
			}
		})
	}
}

func config() session.Config {
	return session.Config{
		Instrument:               mnq,
		StartingBalanceCts:       5_000_000,
		CommissionPerContractCts: 50,
		Rules: challenge.Rules{
			StartingBalanceCts: 5_000_000,
			MaxDailyLossCts:    100_000,
			ProfitTargetCts:    100_000,
			MaxTotalLossCts:    200_000,
		},
	}
}

func drive(t *testing.T, path string) *session.Session {
	t.Helper()
	feed, err := marketdata.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	s, err := session.New(config(), 1_000, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := marketdata.Drive(s, feed); err != nil {
		t.Fatalf("Drive: %v", err)
	}
	return s
}

// Scenario: a change of identifier is a boundary, and the end of the file is
// not
//
//	Given a file with two trading sessions
//	When it drives a session
//	Then the first session is ended and the second opened where the
//	  identifier changes, and the last session is left open, because a
//	  stream running out is not a boundary.
func TestDriveTurnsIdentifierChangesIntoBoundaries(t *testing.T) {
	s := drive(t, "testdata/two-sessions.csv")

	var opened, ended []challenge.SessionID
	for _, e := range s.Events() {
		switch v := e.(type) {
		case session.SessionOpened:
			opened = append(opened, v.SessionID)
		case session.SessionEnded:
			ended = append(ended, v.SessionID)
		}
	}

	if !reflect.DeepEqual(opened, []challenge.SessionID{"d1", "d2"}) {
		t.Fatalf("opened: got %v, want d1 then d2", opened)
	}
	if !reflect.DeepEqual(ended, []challenge.SessionID{"d1"}) {
		t.Fatalf("ended: got %v, want only d1 — the end of a file is not a boundary", ended)
	}
	if s.OpenSessionID() != "d2" {
		t.Fatalf("open session: got %v, want d2 still open", s.OpenSessionID())
	}

	if _, err := session.Replay(s.Events()); err != nil {
		t.Fatalf("the journal a file produced does not replay: %v", err)
	}
}

// Property: the same file produces the same journal, every time.
func TestPropertyTheSameFileProducesTheSameJournal(t *testing.T) {
	const runs = 20

	baseline := drive(t, "testdata/two-sessions.csv").Events()
	for run := 1; run < runs; run++ {
		if !reflect.DeepEqual(drive(t, "testdata/two-sessions.csv").Events(), baseline) {
			t.Fatalf("run %d produced a different journal from the same file", run)
		}
	}

	feed, err := marketdata.ReadFile("testdata/two-sessions.csv")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	again, err := marketdata.ReadFile("testdata/two-sessions.csv")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !reflect.DeepEqual(feed, again) {
		t.Fatal("the same file read twice produced different feeds")
	}
}

// A file with a single row still opens a session and leaves it open.
func TestASingleRowFileDrivesASession(t *testing.T) {
	s := drive(t, "testdata/one-row.csv")

	if s.OpenSessionID() != "d1" {
		t.Fatalf("open session: got %v, want d1", s.OpenSessionID())
	}
	if _, err := session.Replay(s.Events()); err != nil {
		t.Fatalf("Replay: %v", err)
	}
}

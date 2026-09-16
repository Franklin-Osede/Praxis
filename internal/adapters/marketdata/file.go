// Package marketdata reads a canonical Praxis market data file into ordered
// observations, and drives a session with them.
//
// It is an adapter: it touches the filesystem, which no domain package may.
// Nothing in internal/market, internal/execution, internal/portfolio,
// internal/challenge or internal/session imports it.
//
// # The canonical format
//
// A file is CSV with two header lines and then one observation per line:
//
//	praxis.market.v1,MNQ,50
//	time,sequence,session_id,bid,ask,bid_size,ask_size
//	3000,1,d1,20000,20001,10,10
//
// The first line is the format version, the instrument symbol and its
// monetary tick value. The second is the column header, which must match
// exactly — an unknown, missing or reordered column is an error rather than a
// silent misreading. Every field is an integer except the identifiers.
//
// # Why the session identifier is on every row
//
// A trading day is not derived here. It is stated by the file, on every row
// rather than once, so the boundary the domain will consume is visible in the
// data and a misplaced row is detectable immediately. That costs a little
// size and buys two things: the file is auditable on its face, and reading it
// depends on no time zone, no daylight saving rule, no holiday calendar and no
// version of an external calendar.
//
// Converting a provider's raw data into this format is a separate
// responsibility, for a normalizer that will need exactly those external rules
// and should carry its own decision record.
package marketdata

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"

	"praxis/internal/challenge"
	"praxis/internal/market"
)

// Version is the only format this package reads.
const Version = "praxis.market.v1"

var columns = []string{"time", "sequence", "session_id", "bid", "ask", "bid_size", "ask_size"}

// Errors reported for a file that is not a canonical Praxis market file.
var (
	ErrVersion         = errors.New("marketdata: not a recognised file version")
	ErrHeader          = errors.New("marketdata: column header does not match the format")
	ErrTruncated       = errors.New("marketdata: file ends before its data does")
	ErrNoObservations  = errors.New("marketdata: file carries no observations")
	ErrField           = errors.New("marketdata: field is not a valid value")
	ErrOutOfOrder      = errors.New("marketdata: row is not after the one before it")
	ErrSessionReturned = errors.New("marketdata: session identifier reappears after another began")
	ErrEmptySessionID  = errors.New("marketdata: row carries no session identifier")

	// ErrTooLarge reports a file past what a reader will take. The file comes
	// from the operator rather than from a network, which makes it a different
	// risk from a request body and not an unbounded one: a reader with no
	// ceiling reads until the machine stops it, and "the operator would not do
	// that" is not a bound. It is the journal's ceiling, because the two are
	// read together and a limit that differed would be a limit to remember.
	ErrTooLarge = errors.New("marketdata: file is larger than this reader will take")
)

// MaxFileBytes is how much market data one reader will consume.
const MaxFileBytes = 64 << 20

// Observation is one row: a quote and the trading session it belongs to.
type Observation struct {
	SessionID challenge.SessionID
	Sequence  uint64
	Quote     market.Quote
}

// Feed is a whole file: one instrument and its ordered observations.
type Feed struct {
	Instrument   market.Instrument
	Observations []Observation
}

// ReadFile reads a canonical market file.
func ReadFile(path string) (*Feed, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Read(f)
}

// Read parses a canonical market file, rejecting anything it cannot vouch for.
func Read(r io.Reader) (*Feed, error) {
	// Counted as it is parsed rather than read whole first: a file that never
	// ends must be refused without being held.
	bounded := &bounded{from: r, left: MaxFileBytes + 1}
	in := csv.NewReader(bounded)
	in.FieldsPerRecord = -1 // checked per row, to report a short row as truncation

	version, err := in.Read()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrVersion, err)
	}
	instrument, err := parseVersion(version)
	if err != nil {
		return nil, err
	}

	header, err := in.Read()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHeader, err)
	}
	if len(header) != len(columns) {
		return nil, fmt.Errorf("%w: %d columns, want %d", ErrHeader, len(header), len(columns))
	}
	for n, want := range columns {
		if header[n] != want {
			return nil, fmt.Errorf("%w: column %d is %q, want %q", ErrHeader, n, header[n], want)
		}
	}

	feed := &Feed{Instrument: instrument}
	var (
		lastTime     market.LogicalTime
		lastSequence uint64
		started      bool
		seen         []challenge.SessionID
		current      challenge.SessionID
	)

	for row := 3; ; row++ {
		record, err := in.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// A file past the ceiling is its own finding: it did not end early,
			// it did not end.
			if errors.Is(err, ErrTooLarge) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: row %d: %v", ErrTruncated, row, err)
		}
		if len(record) != len(columns) {
			return nil, fmt.Errorf("%w: row %d has %d fields, want %d", ErrTruncated, row, len(record), len(columns))
		}

		o, err := parseRow(instrument, record, row)
		if err != nil {
			return nil, err
		}
		if started && !(o.Quote.Time > lastTime || (o.Quote.Time == lastTime && o.Sequence > lastSequence)) {
			return nil, fmt.Errorf("%w: row %d at (%d, %d) after (%d, %d)",
				ErrOutOfOrder, row, o.Quote.Time, o.Sequence, lastTime, lastSequence)
		}
		lastTime, lastSequence, started = o.Quote.Time, o.Sequence, true

		if o.SessionID != current {
			for _, id := range seen {
				if id == o.SessionID {
					return nil, fmt.Errorf("%w: row %d returns to %s", ErrSessionReturned, row, o.SessionID)
				}
			}
			seen = append(seen, o.SessionID)
			current = o.SessionID
		}
		feed.Observations = append(feed.Observations, o)
	}

	if len(feed.Observations) == 0 {
		return nil, ErrNoObservations
	}
	return feed, nil
}

// bounded is a reader that refuses rather than truncates. io.LimitReader would
// report the end of the file, and a truncated file read as a whole one is a run
// over market nobody chose.
type bounded struct {
	from io.Reader
	left int64
}

func (b *bounded) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, MaxFileBytes)
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.from.Read(p)
	b.left -= int64(n)
	return n, err
}

func parseVersion(record []string) (market.Instrument, error) {
	if len(record) != 3 {
		return market.Instrument{}, fmt.Errorf("%w: %d fields, want 3", ErrVersion, len(record))
	}
	if record[0] != Version {
		return market.Instrument{}, fmt.Errorf("%w: %q, want %q", ErrVersion, record[0], Version)
	}
	centsPerTick, err := market.ParseInt(record[2])
	if err != nil {
		return market.Instrument{}, fmt.Errorf("%w: cents per tick %q: %v", ErrField, record[2], err)
	}
	i := market.Instrument{Symbol: record[1], CentsPerTick: market.Cents(centsPerTick)}
	if err := i.Validate(); err != nil {
		return market.Instrument{}, err
	}
	return i, nil
}

func parseRow(i market.Instrument, record []string, row int) (Observation, error) {
	at, err := parseInt(record[0], "time", row)
	if err != nil {
		return Observation{}, err
	}
	sequence, err := market.ParseUint(record[1])
	if err != nil {
		return Observation{}, fmt.Errorf("%w: row %d sequence %q: %v", ErrField, row, record[1], err)
	}
	if record[2] == "" {
		return Observation{}, fmt.Errorf("%w: row %d", ErrEmptySessionID, row)
	}
	// The identifier is checked here, where the error can name the row, rather
	// than left to the session — which would refuse it much later and blame
	// the boundary instead of the file.
	if err := market.ValidIdentifier(record[2]); err != nil {
		return Observation{}, fmt.Errorf("%w: row %d session identifier: %v", ErrField, row, err)
	}
	bid, err := parseInt(record[3], "bid", row)
	if err != nil {
		return Observation{}, err
	}
	ask, err := parseInt(record[4], "ask", row)
	if err != nil {
		return Observation{}, err
	}
	bidSize, err := parseInt(record[5], "bid_size", row)
	if err != nil {
		return Observation{}, err
	}
	askSize, err := parseInt(record[6], "ask_size", row)
	if err != nil {
		return Observation{}, err
	}

	q := market.Quote{
		Instrument: i,
		Time:       market.LogicalTime(at),
		Bid:        market.Ticks(bid),
		Ask:        market.Ticks(ask),
		BidSize:    market.Qty(bidSize),
		AskSize:    market.Qty(askSize),
	}
	if err := q.Validate(); err != nil {
		return Observation{}, fmt.Errorf("row %d: %w", row, err)
	}
	return Observation{SessionID: challenge.SessionID(record[2]), Sequence: sequence, Quote: q}, nil
}

// parseInt rejects a value that does not fit, so a number too large for the
// domain is an error rather than a wrapped one.
func parseInt(field, name string, row int) (int64, error) {
	v, err := market.ParseInt(field)
	if err != nil {
		return 0, fmt.Errorf("%w: row %d %s %q: %v", ErrField, row, name, field, err)
	}
	return v, nil
}

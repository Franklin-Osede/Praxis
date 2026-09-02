package marketdata

import (
	"errors"
	"fmt"

	"praxis/internal/challenge"
	"praxis/internal/session"
)

// ErrFeedMismatch reports a journal whose observations are not this file's.
var ErrFeedMismatch = errors.New("marketdata: the journal does not describe this market file")

// Consumed reports how many of a feed's rows a journal already contains.
//
// Resuming is not a matter of skipping a count of rows. Every observation the
// journal holds is compared with the row in the same position, field by field
// — instrument, logical time, source sequence, trading session, both sides of
// the book and their sizes. A file that changed while keeping its length would
// otherwise be replayed as though it were the one that produced the journal,
// and the resulting history would describe a market that never happened.
func Consumed(events []session.Event, feed *Feed) (int, error) {
	consumed := 0
	var current challenge.SessionID

	for _, e := range events {
		switch v := e.(type) {
		case session.SessionOpened:
			current = v.SessionID
		case session.SessionEnded:
			current = ""
		case session.MarketObserved:
			if consumed >= len(feed.Observations) {
				return 0, fmt.Errorf("%w: it holds %d observations, the file has %d",
					ErrFeedMismatch, consumed+1, len(feed.Observations))
			}
			row := feed.Observations[consumed]
			if v.Quote != row.Quote || v.SourceSequence != row.Sequence {
				return 0, fmt.Errorf("%w: row %d holds %+v at source sequence %d, the file has %+v at %d",
					ErrFeedMismatch, consumed+1, v.Quote, v.SourceSequence, row.Quote, row.Sequence)
			}
			if current != row.SessionID {
				return 0, fmt.Errorf("%w: row %d was consumed in session %q, the file assigns it to %q",
					ErrFeedMismatch, consumed+1, current, row.SessionID)
			}
			consumed++
		}
	}
	return consumed, nil
}

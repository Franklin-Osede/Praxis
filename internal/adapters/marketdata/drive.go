package marketdata

import (
	"errors"
	"fmt"

	"praxis/internal/challenge"

	"praxis/internal/session"
)

// ErrFeedExhausted reports a step asked for past the last row of a file. It is
// not a boundary — see ADR-011 — and it is not a step either: nothing was shown.
var ErrFeedExhausted = errors.New("marketdata: the file has no row after the last one")

// Drive applies a feed to a session from the given row onwards, opening a
// trading session whenever the identifier changes and ending the previous one
// first.
//
// It starts from the session's own open trading session, so a session resumed
// from a journal carries on rather than reopening a boundary it already
// crossed.
//
// Reaching the end of the file does not close the last trading session. A
// stream running out is not a boundary — see ADR-011 — so the caller decides
// whether the session ended or the data merely stopped.
//
// It is a loop over Step and nothing else. The interface advances one row at a
// time and a driven run advances all of them, and the only way the two cannot
// come apart is for there to be one row.
func Drive(s *session.Session, feed *Feed, from int) error {
	if from < 0 || from > len(feed.Observations) {
		return fmt.Errorf("marketdata: cannot start at row %d of %d", from, len(feed.Observations))
	}
	for at := from; at < len(feed.Observations); {
		// An evaluation that ends is a result and not a failure, so a run stops
		// cleanly rather than erroring. Step refuses the same case; the check
		// is repeated here only so that a driven run reports it as the end of
		// the run rather than as a refusal.
		if s.ChallengeEnded() {
			return nil
		}
		next, err := Step(s, feed, at)
		if err != nil {
			return err
		}
		at = next
	}
	return nil
}

// Step applies one row of a feed to a session, crossing a trading-session
// boundary first if the row's identifier changes, and returns the row to apply
// next.
//
// The open trading session is read from the session on every call rather than
// carried by the caller. That makes a step re-entrant: if one of its commands
// commits and a later one fails, calling it again from the same row does not
// reopen a boundary already crossed.
//
// A row past the end is ErrFeedExhausted and an ended evaluation is
// session.ErrChallengeEnded, and both are refused before anything is written.
// The second is checked before the boundary rather than left to the kernel's
// refusal, because the boundary logic runs first: reacting would close the
// trading session and commit a SessionEnded the policy says should not exist.
func Step(s *session.Session, feed *Feed, from int) (int, error) {
	if from < 0 || from > len(feed.Observations) {
		return from, fmt.Errorf("marketdata: cannot step from row %d of %d", from, len(feed.Observations))
	}
	if from == len(feed.Observations) {
		return from, fmt.Errorf("%w: all %d rows are consumed", ErrFeedExhausted, len(feed.Observations))
	}
	if s.ChallengeEnded() {
		return from, fmt.Errorf("%w: row %d is not applied", session.ErrChallengeEnded, from+1)
	}

	o := feed.Observations[from]
	// The session that is open, and nothing if none is: a name on its own
	// cannot say whether the boundary it belongs to has been crossed.
	var current challenge.SessionID
	if s.TradingSessionOpen() {
		current = s.OpenSessionID()
	}
	if o.SessionID != current {
		if current != "" {
			if err := s.EndTradingSession(o.Quote.Time); err != nil {
				return from, err
			}
		}
		if err := s.OpenTradingSession(o.Quote.Time, o.SessionID); err != nil {
			return from, err
		}
	}
	if err := s.Observe(o.Quote, o.Sequence); err != nil {
		return from, err
	}
	return from + 1, nil
}

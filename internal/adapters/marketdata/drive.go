package marketdata

import (
	"fmt"

	"praxis/internal/challenge"

	"praxis/internal/session"
)

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
func Drive(s *session.Session, feed *Feed, from int) error {
	if from < 0 || from > len(feed.Observations) {
		return fmt.Errorf("marketdata: cannot start at row %d of %d", from, len(feed.Observations))
	}
	// The session that is open, and nothing if none is: a name on its own
	// cannot say whether the boundary it belongs to has been crossed.
	var current challenge.SessionID
	if s.TradingSessionOpen() {
		current = s.OpenSessionID()
	}
	for _, o := range feed.Observations[from:] {
		// Before the boundary, not after it. An evaluation that ends is a
		// result and not a failure, so the run stops cleanly rather than
		// erroring — and it stops here rather than reacting to the kernel's
		// refusal, because the boundary logic runs before the observation
		// does: reacting would close the trading session first and commit a
		// SessionEnded the policy says should not exist.
		if s.ChallengeEnded() {
			return nil
		}
		if o.SessionID != current {
			if current != "" {
				if err := s.EndTradingSession(o.Quote.Time); err != nil {
					return err
				}
			}
			if err := s.OpenTradingSession(o.Quote.Time, o.SessionID); err != nil {
				return err
			}
			current = o.SessionID
		}
		if err := s.Observe(o.Quote, o.Sequence); err != nil {
			return err
		}
	}
	return nil
}

package marketdata

import (
	"praxis/internal/challenge"
	"praxis/internal/session"
)

// Drive applies a feed to a session, opening a trading session whenever the
// identifier changes and ending the previous one first.
//
// Reaching the end of the file does not close the last trading session. A
// stream running out is not a boundary — see ADR-011 — so the caller decides
// whether the session ended or the data merely stopped.
func Drive(s *session.Session, feed *Feed) error {
	var current challenge.SessionID
	for _, o := range feed.Observations {
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
		if err := s.Observe(o.Quote); err != nil {
			return err
		}
	}
	return nil
}

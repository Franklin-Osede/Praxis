package marketdata_test

import (
	"errors"
	"strings"
	"testing"

	"praxis/internal/adapters/marketdata"
	"praxis/internal/session"
)

func feedFile(t *testing.T, body string) *marketdata.Feed {
	t.Helper()
	feed, err := marketdata.Read(strings.NewReader(header + body))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return feed
}

const twoRows = "3000,1,d1,20000,20001,10,10\n4000,1,d1,20010,20011,10,10\n"

func drivenEvents(t *testing.T, feed *marketdata.Feed) []session.Event {
	t.Helper()
	s, err := session.New(config(), 1_000, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := marketdata.Drive(s, feed, 0); err != nil {
		t.Fatalf("Drive: %v", err)
	}
	return s.Events()
}

// Scenario: a journal is matched against the file, not counted against it
func TestConsumedMatchesEveryRow(t *testing.T) {
	feed := feedFile(t, twoRows)
	events := drivenEvents(t, feed)

	consumed, err := marketdata.Consumed(events, feed)
	if err != nil {
		t.Fatalf("Consumed: %v", err)
	}
	if consumed != 2 {
		t.Fatalf("consumed: got %d, want 2", consumed)
	}

	// A file with an extra row resumes from the row after the ones consumed.
	longer := feedFile(t, twoRows+"5000,1,d1,20020,20021,10,10\n")
	consumed, err = marketdata.Consumed(events, longer)
	if err != nil {
		t.Fatalf("Consumed: %v", err)
	}
	if consumed != 2 {
		t.Fatalf("consumed: got %d, want 2", consumed)
	}
}

// Scenario: a file that changed is refused, even where it kept its length
func TestConsumedRefusesAChangedFile(t *testing.T) {
	events := drivenEvents(t, feedFile(t, twoRows))

	tests := []struct {
		name string
		body string
	}{
		{"a changed price", "3000,1,d1,20000,20001,10,10\n4000,1,d1,20099,20100,10,10\n"},
		{"a changed size", "3000,1,d1,20000,20001,10,10\n4000,1,d1,20010,20011,10,99\n"},
		{"a changed time", "3000,1,d1,20000,20001,10,10\n4999,1,d1,20010,20011,10,10\n"},
		{"a changed source sequence", "3000,1,d1,20000,20001,10,10\n4000,7,d1,20010,20011,10,10\n"},
		{"a changed session", "3000,1,d1,20000,20001,10,10\n4000,1,d2,20010,20011,10,10\n"},
		{"a row removed", "3000,1,d1,20000,20001,10,10\n"},
		{"the rows reordered", "3000,1,d1,20010,20011,10,10\n4000,1,d1,20000,20001,10,10\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := marketdata.Consumed(events, feedFile(t, tc.body)); !errors.Is(err, marketdata.ErrFeedMismatch) {
				t.Fatalf("error: got %v, want %v", err, marketdata.ErrFeedMismatch)
			}
		})
	}
}

// Two rows sharing a logical time are told apart only by their source
// sequence, so swapping them must be caught.
func TestConsumedCatchesSwappedRowsAtTheSameTime(t *testing.T) {
	const original = "3000,1,d1,20000,20001,10,10\n3000,2,d1,20005,20006,10,10\n"
	const swapped = "3000,1,d1,20005,20006,10,10\n3000,2,d1,20000,20001,10,10\n"

	events := drivenEvents(t, feedFile(t, original))
	if _, err := marketdata.Consumed(events, feedFile(t, swapped)); !errors.Is(err, marketdata.ErrFeedMismatch) {
		t.Fatalf("error: got %v, want %v", err, marketdata.ErrFeedMismatch)
	}
}

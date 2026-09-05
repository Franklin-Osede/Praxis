package session

import (
	"errors"
	"testing"

	"praxis/internal/market"
)

// Scenario: a position must be strictly after the last one, in both dimensions
//
// ValidateNext is what a command asks before it lets any aggregate decide, so
// that an evaluation cannot accept a boundary the journal then refuses. Both
// halves of "strictly after" are load-bearing and neither had a test: the
// package's only ordering assertion goes through a command and exercises the
// time dimension from far enough away that a relaxed comparison still passes.
func TestValidateNextRefusesAnythingNotStrictlyAfter(t *testing.T) {
	j := &Journal{}
	if err := j.Append(SessionEnded{Envelope: Envelope{Time: 1_000, Sequence: 7, Kind: KindSessionEnded}}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	tests := []struct {
		name     string
		at       market.LogicalTime
		sequence uint64
		want     error
	}{
		{"a later time", 1_001, 8, nil},
		{"a later time and a lower sequence", 1_001, 1, nil},
		{"the same time and the next sequence", 1_000, 8, nil},
		{"the same position exactly", 1_000, 7, ErrOutOfOrder},
		{"the same time and the same sequence, again", 1_000, 7, ErrOutOfOrder},
		{"the same time and a lower sequence", 1_000, 6, ErrOutOfOrder},
		{"an earlier time", 999, 8, ErrOutOfOrder},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := j.ValidateNext(tc.at, tc.sequence); !errors.Is(err, tc.want) {
				t.Fatalf("error: got %v, want %v", err, tc.want)
			}
			// Asking must never change the answer to asking again.
			if err := j.ValidateNext(tc.at, tc.sequence); !errors.Is(err, tc.want) {
				t.Fatalf("a second ask answered differently: %v", err)
			}
		})
	}

	// An empty journal accepts anything, which is what starting means.
	if err := (&Journal{}).ValidateNext(0, 0); err != nil {
		t.Fatalf("an empty journal refused a first position: %v", err)
	}
}

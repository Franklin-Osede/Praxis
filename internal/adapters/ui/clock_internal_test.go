package ui

import (
	"testing"
	"time"
)

// Scenario: a corrected wall clock does not move an interval
//
//	Given a clock whose wall reading is set backwards mid-segment while its
//	  monotonic count goes forward
//	Then the elapsed a stamp carries still goes forward, and the wall reading
//	  is recorded as it was given.
//
// This is the scenario §11 names — a time server corrects it, an operator sets
// it, a machine resumes — and the consequence of getting it wrong is not a
// slightly dirty measurement. An elapsed computed from the wall would go
// backwards, checkOrder would refuse the command, and Apply only runs after a
// successful record, so the session's high reading would not move: every act
// for the next 500ms refused with "your reading went backwards", to a
// participant who did nothing wrong, and then recovering on its own with
// nobody able to say why.
//
// It is testable because the clock hands back the two readings separately. It
// was not while the seam was a func() time.Time: a fixture's time.Time carries
// no monotonic reading, so subtracting was the wall difference either way.
func TestACorrectedWallClockDoesNotMoveAnInterval(t *testing.T) {
	var step int
	corrected := Clock(func() Reading {
		step++
		// A time server sets the wall two seconds back between the two stamps,
		// so it really does go backwards. The monotonic count knows nothing
		// about that and keeps counting.
		wall := time.Unix(0, 1_764_000_000_000_000_000).Add(time.Duration(step) * time.Second)
		if step > 2 {
			wall = wall.Add(-2 * time.Second)
		}
		return Reading{Wall: wall, Mono: time.Duration(step) * time.Second}
	})

	l := newLease(0, corrected)
	token, _, err := l.acquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	first, err := l.stamp(token)
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
	second, err := l.stamp(token)
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}

	if second.ElapsedNanos <= first.ElapsedNanos {
		t.Fatalf("elapsed went from %d to %d while the monotonic count advanced: "+
			"the interval is being measured on the wall clock",
			first.ElapsedNanos, second.ElapsedNanos)
	}
	// And the wall reading is recorded as given, corrections and all: it is for
	// audit, and refusing a corrected clock would refuse an honest session.
	if second.AtUTCNanos >= first.AtUTCNanos {
		t.Fatalf("the fixture did not set the wall back, so this proves nothing: %d then %d",
			first.AtUTCNanos, second.AtUTCNanos)
	}
}

package ui

import (
	"errors"
	"testing"
)

// Scenario: a request from a lease that has been replaced is refused
//
// Exclusive control is this package's invariant and not the journal's. The rule
// that a segment never reappears keeps the log's chronology coherent; it says
// nothing about who was holding the controls, because two clients sharing one
// token could interleave their gestures inside a single segment without
// breaking it. So the lease is tested here, on the thing that enforces it.
func TestALeaseThatHasBeenReplacedNoLongerControls(t *testing.T) {
	l := newLease(0, nil)

	first, firstSegment, err := l.acquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if got, err := l.check(first); err != nil || got != firstSegment {
		t.Fatalf("check: got %d, %v", got, err)
	}

	// A second client is refused while somebody holds them.
	if _, _, err := l.acquire(); !errors.Is(err, ErrControllerActive) {
		t.Fatalf("acquire: got %v, want %v", err, ErrControllerActive)
	}

	key, err := l.mintHandover()
	if err != nil {
		t.Fatalf("mintHandover: %v", err)
	}
	second, secondSegment, _, err := l.transfer(key)
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if secondSegment <= firstSegment {
		t.Fatalf("segments: %d then %d, want the second higher", firstSegment, secondSegment)
	}

	// The request that was already in flight when control changed hands is
	// refused on arrival, not by when it was sent.
	if _, err := l.check(first); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("a replaced lease still controls: %v", err)
	}
	if _, err := l.check(second); err != nil {
		t.Fatalf("the current lease was refused: %v", err)
	}
	// And so is an invented one.
	if _, err := l.check("not-a-lease"); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("an invented lease controlled: %v", err)
	}
}

// Reconnecting on the same token keeps the segment. Losing the view is not
// losing the controls, and a new segment would say a run of interaction ended
// when only a socket did.
func TestReconnectingOnTheSameLeaseKeepsItsSegment(t *testing.T) {
	l := newLease(3, nil)
	token, segment, err := l.acquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if segment != 4 {
		t.Fatalf("segment: got %d, want 4 — above every segment the journal holds", segment)
	}
	for i := 0; i < 3; i++ {
		got, err := l.check(token)
		if err != nil || got != segment {
			t.Fatalf("reconnecting moved the segment to %d: %v", got, err)
		}
	}
}

// A lease taken and given up with no events produced leaves nothing behind. No
// decisions were made in it, so there is no segment to record — and the next
// one continues from where the journal actually is.
func TestALeaseAbandonedWithoutDecisionsLeavesNothing(t *testing.T) {
	l := newLease(0, nil)
	token, first, err := l.acquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := l.release(token); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := l.check(token); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("a released lease still controls: %v", err)
	}

	// The controls are free again, and the next lease is a new run of
	// interaction rather than a resumption of the abandoned one.
	_, second, err := l.acquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if second <= first {
		t.Fatalf("segments: %d then %d, want the second higher", first, second)
	}
	if err := l.release("not-a-lease"); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("an invented lease released the controls: %v", err)
	}
}

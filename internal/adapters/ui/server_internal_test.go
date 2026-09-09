package ui

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"praxis/internal/adapters/persistence"
)

// Scenario: closing twice is not a crash
//
//	Given a server that has been closed
//	When it is closed again
//	Then nothing panics.
//
// Close is called from a defer, from a signal handler and from a test cleanup,
// and any two of those can run. A close of a closed channel is a panic, which
// takes the process down while it is shutting down cleanly — the one moment a
// journal is most likely to be mid-commit.
func TestClosingTwiceIsNotACrash(t *testing.T) {
	w, err := persistence.OpenWriter(filepath.Join(t.TempDir(), "journal.praxis"), persistence.DurableEveryBatch)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	s := &Server{commands: make(chan func()), done: make(chan struct{}), writer: w}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("the second Close panicked: %v", r)
		}
	}()
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// Scenario: a request the loop cannot serve spends nothing
//
//	Given a server whose loop is gone
//	When a client asks for the controls
//	Then it is refused, no segment is spent and no lease is held.
//
// Segments only go up, and one is spent to say "a new run of uninterrupted
// interaction begins here". Minting one for a request that is then refused
// records a run that never happened, and leaves the controls held by a client
// that was told it failed.
func TestTakingTheControlsWhenTheLoopIsGoneSpendsNothing(t *testing.T) {
	s := &Server{commands: make(chan func()), done: make(chan struct{})}
	s.lease = newLease(4)
	close(s.done) // the loop is not running

	w := httptest.NewRecorder()
	s.handleControl(w, httptest.NewRequest(http.MethodPost, "/api/control", nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	if s.lease.token != "" {
		t.Fatalf("the controls are held by a client that was refused: %q", s.lease.token)
	}
	if s.lease.highest != 4 {
		t.Fatalf("segment: got %d, want the 4 it started at", s.lease.highest)
	}
}

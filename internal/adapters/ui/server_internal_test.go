package ui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"praxis/internal/adapters/persistence"
	"praxis/internal/session"
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
	s.lease = newLease(4, nil)
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

// Scenario: closing waits for the command the loop already accepted
//
//	Given a command the loop has accepted and is still running
//	When the server is closed
//	Then Close does not return, and the journal is not closed, until the
//	  command has finished — and its commit succeeds.
//
// A signal arriving mid-commit used to close the writer under the command, and
// the command's append failed against a closed file: a torn tail, recoverable,
// and a pilot session interrupted for no reason but the order of two lines.
// What the loop received it finishes; what it did not receive is refused.
func TestCloseWaitsForTheCommandTheLoopAccepted(t *testing.T) {
	dir := t.TempDir()
	marketPath := filepath.Join(dir, "market.csv")
	if err := os.WriteFile(marketPath, []byte("praxis.market.v1,MNQ,50\n"+
		"time,sequence,session_id,bid,ask,bid_size,ask_size\n"+
		"3000,1,d1,20000,20001,10,10\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	s, err := Open(Options{Market: marketPath, Journal: filepath.Join(dir, "journal.praxis"), New: internalPilotConfig()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	go s.Serve()

	entered, release := make(chan struct{}), make(chan struct{})
	appended := make(chan error, 1)
	go func() {
		_ = s.ask(func() {
			close(entered)
			<-release
			// A commit, straight to the writer: what matters is whether the
			// file is still open when an accepted command reaches it.
			_, err := s.writer.Append([]session.Event{session.SessionOpened{
				Envelope:  session.Envelope{Time: 9_000, Sequence: s.writer.NextSequence(), Kind: session.KindSessionOpened},
				SessionID: "late", BalanceCts: 5_000_000, EquityCts: 5_000_000,
			}})
			appended <- err
		})
	}()
	<-entered

	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned (%v) while the loop was still running a command it had accepted", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	if err := <-appended; err != nil {
		t.Fatalf("the accepted command's commit failed: %v", err)
	}
	if err := <-closed; err != nil {
		t.Fatalf("Close: %v", err)
	}

	// And nothing reaches a loop that has stopped: a later request is refused
	// on the done branch rather than left waiting for a loop that is gone.
	if err := s.ask(func() { t.Error("a command ran after Close") }); err == nil {
		t.Fatal("a request after Close was accepted")
	}
}

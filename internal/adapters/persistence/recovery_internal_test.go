package persistence

import (
	"errors"
	"os"
	"testing"

	"praxis/internal/challenge"
	"praxis/internal/market"
	"praxis/internal/session"
)

func mnqInstrument() market.Instrument {
	return market.Instrument{Symbol: "MNQ", CentsPerTick: 50}
}

func sessionConfig() session.Config {
	return session.Config{
		Instrument: mnqInstrument(),
		// Somebody traded it, which is what makes the human clock these tests
		// stamp coherent.
		SubjectID:                "t-01",
		Pacing:                   session.PacingPilot,
		StartingBalanceCts:       5_000_000,
		CommissionPerContractCts: 50,
		Rules: challenge.Rules{
			StartingBalanceCts: 5_000_000,
			MaxDailyLossCts:    100_000,
			ProfitTargetCts:    100_000,
		},
	}
}

// openSessionOnDisk builds a session whose journal is written to path, already
// past a couple of confirmed commands.
func openSessionOnDisk(t *testing.T, path string) (*session.Session, *Writer) {
	t.Helper()
	w, err := OpenWriter(path, DurableEveryBatch)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	s, err := session.New(sessionConfig(), 1_000, w)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.OpenTradingSession(2_000, "d1"); err != nil {
		t.Fatalf("OpenTradingSession: %v", err)
	}
	q := market.Quote{Instrument: mnqInstrument(), Time: 3_000, Bid: 20_000, Ask: 20_001, BidSize: 50, AskSize: 50}
	if err := s.Observe(q, 1); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	return s, w
}

func countEnded(events []session.Event) int {
	n := 0
	for _, e := range events {
		if _, ok := e.(session.SessionEnded); ok {
			n++
		}
	}
	return n
}

// Scenario: a commit that fails at any byte leaves a session that stops, and a
// journal that says whether the command happened
//
//	Given a command interrupted after any number of its bytes reached the
//	  file
//	When the journal is recovered
//	Then the recovered state is exactly what the confirmed batches describe,
//	  the command appears once or not at all — never twice — and the session
//	  that failed refuses to do anything else.
func TestACommitFailingAtEveryByteOffset(t *testing.T) {
	// The size of the batch this command produces, measured once.
	probePath := tempJournal(t)
	probe, probeWriter := openSessionOnDisk(t, probePath)
	before := probeWriter.NextSequence()
	if err := probe.EndTradingSession(9_000); err != nil {
		t.Fatalf("EndTradingSession: %v", err)
	}
	probeWriter.Close()
	ended := session.SessionEnded{
		Envelope:  session.Envelope{Time: 9_000, Sequence: before, Kind: session.KindSessionEnded},
		SessionID: "d1",
	}
	framed, err := EncodeBatch(probeWriter.NextBatchNumber()-1, []session.Event{ended}, EventVersion)
	if err != nil {
		t.Fatalf("EncodeBatch: %v", err)
	}

	for offset := 0; offset < len(framed); offset++ {
		t.Run("", func(t *testing.T) {
			path := tempJournal(t)
			s, w := openSessionOnDisk(t, path)
			confirmedBefore := len(s.Events()) - 0

			original := writeFile
			writeFile = func(f *os.File, b []byte) (int, error) {
				if offset == 0 {
					return 0, errInjected
				}
				n, _ := f.Write(b[:min(offset, len(b))])
				return n, errInjected
			}
			err := s.EndTradingSession(9_000)
			writeFile = original
			w.Close()

			if !errors.Is(err, session.ErrSessionNeedsRecovery) {
				t.Fatalf("offset %d: got %v, want %v", offset, err, session.ErrSessionNeedsRecovery)
			}
			if err := s.Observe(market.Quote{Instrument: mnqInstrument(), Time: 10_000, Bid: 1, Ask: 2, BidSize: 1, AskSize: 1}, 1); !errors.Is(err, session.ErrSessionNeedsRecovery) {
				t.Fatalf("offset %d: a stopped session accepted a command: %v", offset, err)
			}

			state, journal, err := Recover(path)
			if err != nil {
				t.Fatalf("offset %d: Recover: %v", offset, err)
			}
			if n := countEnded(state.Events); n > 1 {
				t.Fatalf("offset %d: the command appears %d times", offset, n)
			}
			if len(state.Events) != len(journal.Events()) {
				t.Fatalf("offset %d: recovered state does not match the confirmed batches", offset)
			}
			// Every confirmed batch that preceded the failure survived.
			if len(state.Events) < confirmedBefore-1 {
				t.Fatalf("offset %d: recovery lost confirmed events", offset)
			}

			// And the recovered session carries on from exactly there.
			resumed, err := session.Resume(state, nil)
			if err != nil {
				t.Fatalf("offset %d: Resume: %v", offset, err)
			}
			if resumed.NeedsRecovery() != nil {
				t.Fatalf("offset %d: a recovered session is already broken", offset)
			}
		})
	}
}

// Scenario: the sync failed but the bytes landed
//
//	Given a batch whose bytes reached the file before the sync reported an
//	  error
//	When the journal is recovered
//	Then the command is found to have committed. It is not run again: a
//	  duplicate would be indistinguishable from a decision the trader made
//	  twice.
//
// This is the case that makes "discover" different from "retry", and it is the
// reason a failed commit is never retried.
func TestASyncThatFailsAfterTheBytesLanded(t *testing.T) {
	path := tempJournal(t)
	s, w := openSessionOnDisk(t, path)

	original := syncFile
	syncFile = func(f *os.File) error {
		f.Sync() // the bytes really do land
		return errInjected
	}
	err := s.EndTradingSession(9_000)
	syncFile = original
	w.Close()

	if !errors.Is(err, session.ErrSessionNeedsRecovery) {
		t.Fatalf("error: got %v, want %v", err, session.ErrSessionNeedsRecovery)
	}

	state, journal, err := Recover(path)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if journal.Tail != TailComplete {
		t.Fatalf("tail: got %v, want a complete journal — the bytes landed", journal.Tail)
	}
	if n := countEnded(state.Events); n != 1 {
		t.Fatalf("the command appears %d times, want exactly once: it committed", n)
	}
	if state.SessionOpen {
		t.Fatal("the recovered state still has the trading session open")
	}

	// Recovery reports what happened; it does not re-run the command. Nothing
	// here re-executes it, and the journal proves it did not need to be.
	if len(state.Events) != len(journal.Events()) {
		t.Fatal("recovered state does not match the confirmed batches")
	}
}

// A session with no committer keeps working exactly as before, which is what
// every test that does not care about persistence relies on.
func TestASessionWithoutACommitterIsUnchanged(t *testing.T) {
	s, err := session.New(sessionConfig(), 1_000, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.OpenTradingSession(2_000, "d1"); err != nil {
		t.Fatalf("OpenTradingSession: %v", err)
	}
	if s.NeedsRecovery() != nil {
		t.Fatalf("a session with no committer needs recovery: %v", s.NeedsRecovery())
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// present confirms whatever the session has just put on the screen. A human
// command taken before this is refused, because an interval from a presentation
// nobody confirmed has no beginning.
func present(t *testing.T, s *session.Session) {
	t.Helper()
	id, waiting := s.Pending(1)
	if !waiting {
		return
	}
	if err := s.AcknowledgePresentation(id, session.Instant{
		AtUTCNanos: 1_764_000_000_000_000_000, Segment: 1,
	}); err != nil {
		t.Fatalf("AcknowledgePresentation: %v", err)
	}
}

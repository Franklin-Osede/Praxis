package ui_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"praxis/internal/adapters/marketdata"
	"praxis/internal/adapters/persistence"
	"praxis/internal/adapters/ui"
	"praxis/internal/market"
	"praxis/internal/session"
)

// tickingClock hands out a fixed sequence, so the same run twice is the same
// run. It is what makes the comparison below possible at all: with the stamps
// coming from the server, a reproducible journal needs a reproducible clock.
func tickingClock() func() time.Time {
	base := time.Unix(0, 1_764_000_000_000_000_000).UTC()
	var n int64
	return func() time.Time {
		n++
		return base.Add(time.Duration(n) * time.Millisecond)
	}
}

// Scenario: a journal written through the interface is a journal
//
//	Given the same acts taken through HTTP and taken directly
//	Then the two journals are byte for byte identical.
//
// It is the analogue of the CLI's resumed-run comparison for the human edge,
// and it is what makes "the interface is an adapter" a fact rather than an
// intention: everything the participant does has to arrive at the kernel as the
// same commands a test would call, and leave the same bytes behind.
//
// The comparison is only possible because the server stamps and the clock is
// injected. If the browser stamped, the two runs would carry whatever it sent
// and this test would be comparing the fixture to itself.
func TestAJournalWrittenThroughTheInterfaceIsAJournal(t *testing.T) {
	dir := t.TempDir()
	marketPath := filepath.Join(dir, "market.csv")
	if err := os.WriteFile(marketPath, []byte(thinBook), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	overHTTP := filepath.Join(dir, "http.praxis")
	segment := driveThroughHTTP(t, marketPath, overHTTP)

	direct := filepath.Join(dir, "direct.praxis")
	driveDirectly(t, marketPath, direct, segment)

	got, err := os.ReadFile(overHTTP)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want, err := os.ReadFile(direct)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("the two journals differ\n through HTTP:\n%s\n directly:\n%s", got, want)
	}
}

// driveThroughHTTP takes the controls, confirms the observation and submits an
// order with protection, and returns the segment the server issued.
func driveThroughHTTP(t *testing.T, marketPath, journalPath string) uint64 {
	t.Helper()
	writeJournal(t, marketPath, journalPath, pilotConfig(), false)

	s, err := ui.Open(ui.Options{
		Market: marketPath, Journal: journalPath, Now: tickingClock(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	go s.Serve()
	defer s.Close()

	control := takeControl(t, s)
	segment, err := parseUint(control.Segment)
	if err != nil {
		t.Fatal(err)
	}

	ack := acknowledge(t, s, map[string]string{
		"lease": control.Lease, "observedSequence": observedSequenceOf(t, s),
	})
	defer ack.Body.Close()
	if ack.StatusCode != http.StatusOK {
		t.Fatalf("acknowledging: status %d, reason %q", ack.StatusCode, reasonOf(t, ack))
	}

	resp := postCommand(t, s, map[string]string{
		"kind": "submit_order", "lease": control.Lease,
		"gesture": gestureName(segment, 1),
		"orderId": "e-1", "side": "buy", "type": "market", "qty": "2",
		"protectionStop": "19000", "protectionTarget": "21000",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submitting: status %d, reason %q", resp.StatusCode, reasonOf(t, resp))
	}
	return segment
}

// driveDirectly performs the same acts against the session, stamped with the
// same clock the server would have used.
func driveDirectly(t *testing.T, marketPath, journalPath string, segment uint64) {
	t.Helper()
	writeJournal(t, marketPath, journalPath, pilotConfig(), false)

	feed, err := marketdata.ReadFile(marketPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	w, err := persistence.OpenWriter(journalPath, persistence.DurableEveryBatch)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	defer w.Close()

	state, err := session.Replay(w.Recovered().Events())
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	s, err := session.Resume(state, w)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	// The same clock, at the same points: the lease is granted, then the
	// acknowledgement arrives, then the command does.
	clock := tickingClock()
	clock() // granting the lease
	ackAt := instantAt(clock(), segment)
	if err := s.AcknowledgePresentation(
		session.PresentationID{Segment: segment, ObservedSequence: s.LastObserved()}, ackAt,
	); err != nil {
		t.Fatalf("AcknowledgePresentation: %v", err)
	}
	commandAt := instantAt(clock(), segment)

	entry, err := market.NewMarketOrder("e-1", feed.Instrument, market.SideBuy, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitOrderWithProtection(entry, 19_000, 21_000, session.Decision{
		GestureID:    gestureName(segment, 1),
		AtUTCNanos:   commandAt.AtUTCNanos,
		Segment:      commandAt.Segment,
		ElapsedNanos: commandAt.ElapsedNanos,
	}); err != nil {
		t.Fatalf("SubmitOrderWithProtection: %v", err)
	}
}

// instantAt is the stamp the lease would have produced: the wall reading, and
// the elapsed measured from the moment the lease was granted.
func instantAt(at time.Time, segment uint64) session.Instant {
	granted := time.Unix(0, 1_764_000_000_000_000_000).UTC().Add(time.Millisecond)
	return session.Instant{
		AtUTCNanos:   session.UnixNanos(at.UnixNano()),
		Segment:      segment,
		ElapsedNanos: session.ElapsedNanos(at.Sub(granted).Nanoseconds()),
	}
}

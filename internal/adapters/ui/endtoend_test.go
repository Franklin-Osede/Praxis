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
func tickingClock() ui.Clock {
	base := time.Unix(0, 1_764_000_000_000_000_000).UTC()
	var n int64
	return func() ui.Reading {
		n++
		return ui.Reading{
			Wall: base.Add(time.Duration(n) * time.Millisecond),
			Mono: time.Duration(n) * time.Millisecond,
		}
	}
}

// Scenario: a journal written through the interface is a journal
//
//	Given the same acts taken through HTTP and taken directly, from an empty
//	  journal and the first row of the file
//	Then the two journals are byte for byte identical.
//
// It is the analogue of the CLI's resumed-run comparison for the human edge,
// and it is what makes "the interface is an adapter" a fact rather than an
// intention: everything the participant does has to arrive at the kernel as the
// same commands a test would call, and leave the same bytes behind.
//
// It starts from nothing and advances by steps, interleaved with confirmations
// and a command. It used to start from a journal already driven to the end of
// its file, and so it compared the one state no participant is ever in: the
// advance, the boundary logic and the confirmation between steps were not
// covered at all, which is how a server that could not move the market passed.
//
// The comparison is only possible because the server stamps and the clock is
// injected. If the browser stamped, the two runs would carry whatever it sent
// and this test would be comparing the fixture to itself.
func TestAJournalWrittenThroughTheInterfaceIsAJournal(t *testing.T) {
	dir := t.TempDir()
	marketPath := filepath.Join(dir, "market.csv")
	if err := os.WriteFile(marketPath, []byte(marketFile), 0o600); err != nil {
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

// driveThroughHTTP opens an empty journal, takes the controls, and walks the file:
// a step, a confirmation, an order with protection, another step, another
// confirmation. It returns the segment the server issued.
func driveThroughHTTP(t *testing.T, marketPath, journalPath string) uint64 {
	t.Helper()
	s, err := ui.Open(ui.Options{
		Market: marketPath, Journal: journalPath, New: pilotConfig(), Now: tickingClock(),
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

	first := stepOK(t, s, control.Lease, "")
	confirm(t, s, control.Lease, first.ObservedSequence)

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

	second := stepOK(t, s, control.Lease, first.ObservedSequence)
	if second.Cursor != second.Observations {
		t.Fatalf("the walk did not reach the end of the file: %+v", second)
	}
	confirm(t, s, control.Lease, second.ObservedSequence)
	return segment
}

// driveDirectly performs the same acts against a session built the way the
// server builds one, stamped with the clock the server would have used, and
// advanced by the same function the server calls.
func driveDirectly(t *testing.T, marketPath, journalPath string, segment uint64) {
	t.Helper()
	feed, err := marketdata.ReadFile(marketPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	w, err := persistence.OpenWriter(journalPath, persistence.DurableEveryBatch)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	defer w.Close()

	cfg := pilotConfig()
	cfg.Instrument = feed.Instrument
	s, err := session.New(cfg, feed.Observations[0].Quote.Time, w)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The same clock, at the same points. A step stamps nothing — it is not a
	// decision — so only the lease, the confirmations and the command read it.
	clock := tickingClock()
	clock() // granting the lease

	stepTo := func(row int) {
		t.Helper()
		if _, err := marketdata.Step(s, feed, row); err != nil {
			t.Fatalf("Step(%d): %v", row, err)
		}
	}
	confirmHere := func() {
		t.Helper()
		if err := s.AcknowledgePresentation(
			session.PresentationID{Segment: segment, ObservedSequence: s.LastObserved()},
			instantAt(clock(), segment),
		); err != nil {
			t.Fatalf("AcknowledgePresentation: %v", err)
		}
	}

	stepTo(0)
	confirmHere()

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

	stepTo(1)
	confirmHere()
}

// instantAt is the stamp the lease would have produced: the wall reading, and
// the elapsed measured from the moment the lease was granted.
// instantAt is the stamp the lease would have produced: the wall reading as
// given, and the elapsed measured from the monotonic count the lease was
// granted at — which is the first tick.
func instantAt(at ui.Reading, segment uint64) session.Instant {
	const granted = time.Millisecond
	return session.Instant{
		AtUTCNanos:   session.UnixNanos(at.Wall.UnixNano()),
		Segment:      segment,
		ElapsedNanos: session.ElapsedNanos((at.Mono - granted).Nanoseconds()),
	}
}

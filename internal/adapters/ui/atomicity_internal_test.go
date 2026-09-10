package ui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"praxis/internal/adapters/marketdata"
	"praxis/internal/market"
	"praxis/internal/session"
)

// Scenario: one command is one turn of the loop
//
//	Given a command that finds the gesture, checks the lease and executes
//	Then all of it happens in a single closure on the loop.
//
// This is asserted structurally, and that is a correction rather than a
// preference. The concurrent test next door — eight clients retrying one
// gesture, and one order in the journal — proves something real but does not
// prove this: splitting the lookup and the execution into two turns survives it
// 10 times out of 10, at 64 clients as well as at 8. The interleaving the split
// makes possible is not one the Go scheduler produces through an HTTP round
// trip often enough to be a test, and a test that catches a defect
// occasionally is worse than one that says what it checks.
//
// So the property is checked where it is decidable: the loop is the only
// serialisation there is, so "find, check and execute cannot be interleaved"
// and "they are one closure" are the same statement, and the second is
// countable.
func TestOneCommandIsOneTurnOfTheLoop(t *testing.T) {
	// Built by hand rather than opened, because Open starts the real loop and
	// the count has to come from the only reader of the channel.
	cfg := internalPilotConfig()
	feed, err := marketdata.Read(strings.NewReader(oneRowMarket))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	cfg.Instrument = feed.Instrument
	sess, err := session.New(cfg, feed.Observations[0].Quote.Time, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := marketdata.Drive(sess, feed, 0); err != nil {
		t.Fatalf("Drive: %v", err)
	}
	s := &Server{
		session: sess, cfg: cfg, feed: feed, cursor: len(feed.Observations),
		lease:    newLease(0),
		commands: make(chan func()), done: make(chan struct{}),
	}
	defer close(s.done)

	// The loop, counting the closures it is given.
	var turns atomic.Int64
	go func() {
		for {
			select {
			case run := <-s.commands:
				turns.Add(1)
				run()
			case <-s.done:
				return
			}
		}
	}()

	token, segment, err := s.lease.acquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// The observation has to be confirmed before a human command is taken.
	if err := s.session.AcknowledgePresentation(
		session.PresentationID{Segment: segment, ObservedSequence: s.session.LastObserved()},
		session.Instant{AtUTCNanos: 1_764_000_000_000_000_000, Segment: segment, ElapsedNanos: 1_000},
	); err != nil {
		t.Fatalf("AcknowledgePresentation: %v", err)
	}

	before := turns.Load()
	w := httptest.NewRecorder()
	s.handleCommand(w, httptest.NewRequest(http.MethodPost, "/api/command",
		bytes.NewReader([]byte(`{"kind":"submit_order","lease":"`+token+`",`+
			`"gesture":"`+market.FormatUint(segment)+`:1","atUtcNanos":"1764000000000000001","elapsedNanos":"2000",`+
			`"orderId":"o-1","side":"buy","type":"market","qty":"1"}`))))

	if got := turns.Load() - before; got != 1 {
		t.Fatalf("a command took %d turns of the loop, want 1: anything more is a window "+
			"another request can act in", got)
	}
}

const oneRowMarket = "praxis.market.v1,MNQ,50\n" +
	"time,sequence,session_id,bid,ask,bid_size,ask_size\n" +
	"3000,1,d1,20000,20001,10,10\n"

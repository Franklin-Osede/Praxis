package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"praxis/internal/adapters/marketdata"
	"praxis/internal/market"
	"praxis/internal/session"
)

// A commit that fails is terminal, and the whole of this apparatus rests on one
// sentence: what the journal says is committed is committed, and stays
// committed. These scenarios are the first half of it — the interface must not
// say a command succeeded when the store never saw it.
//
// The kernel is not what is under test here. It stops correctly: ADR-012 lets
// the aggregates mutate before the commit is attempted, and session.command
// records the failure and refuses everything afterwards. What is under test is
// the adapter, which answers three questions out of its own memory before it
// ever asks the kernel — is this gesture a retry, is this gesture a reuse, is
// this step a retry — and that memory was written by a command the disk never
// took.

// stopsOnDemand is a committer that keeps every batch until it is told to fail,
// and refuses every batch after. It is the disk going away in the middle of a
// run: the events the command produced never reach it, and the session stops.
type stopsOnDemand struct {
	failing  bool
	attempts int
	batches  [][]session.Event
}

var errJournalGone = errors.New("the journal is gone")

func (c *stopsOnDemand) Commit(events []session.Event) error {
	c.attempts++
	if c.failing {
		return errJournalGone
	}
	c.batches = append(c.batches, append([]session.Event(nil), events...))
	return nil
}

const threeRowMarket = "praxis.market.v1,MNQ,50\n" +
	"time,sequence,session_id,bid,ask,bid_size,ask_size\n" +
	"3000,1,d1,20000,20001,10,10\n" +
	"4000,2,d1,20010,20011,10,10\n" +
	"5000,3,d1,20020,20021,10,10\n"

// onARunAboutToLoseItsJournal opens a server by hand over a committer the test
// controls, confirms the first row, and hands back the lease. It is built
// rather than opened because Open commits to a file, and a file cannot be made
// to fail on the row the test chooses.
func onARunAboutToLoseItsJournal(t *testing.T) (*Server, *stopsOnDemand, string, uint64) {
	t.Helper()

	cfg := internalPilotConfig()
	feed, err := marketdata.Read(strings.NewReader(threeRowMarket))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	cfg.Instrument = feed.Instrument

	committer := &stopsOnDemand{}
	sess, err := session.New(cfg, feed.Observations[0].Quote.Time, committer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// One row only, so that a step still has somewhere to go.
	cursor, err := marketdata.Step(sess, feed, 0)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}

	s := &Server{
		session: sess, cfg: cfg, feed: feed, cursor: cursor,
		lease:    newLease(0, nil),
		commands: make(chan func()), done: make(chan struct{}), stopped: make(chan struct{}),
	}
	go s.loop()
	t.Cleanup(func() { close(s.done); <-s.stopped })

	token, segment, err := s.lease.acquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// The row on the screen has to be confirmed before the market moves past it
	// and before a human command is taken.
	if err := s.session.AcknowledgePresentation(
		session.PresentationID{Segment: segment, ObservedSequence: s.session.LastObserved()},
		session.Instant{AtUTCNanos: 1_764_000_000_000_000_000, Segment: segment, ElapsedNanos: 1_000},
	); err != nil {
		t.Fatalf("AcknowledgePresentation: %v", err)
	}
	return s, committer, token, segment
}

func submitOrder(token, gesture, orderID string) string {
	return `{"kind":"submit_order","lease":"` + token + `","gesture":"` + gesture + `",` +
		`"orderId":"` + orderID + `","side":"buy","type":"market","qty":"1"}`
}

func stepFrom(token, observed string) string {
	return `{"lease":"` + token + `","fromObservedSequence":"` + observed + `"}`
}

// answered posts one body to a handler and reports what a client reads: the
// status, and the reason, which is the field the specification says a client
// discriminates on. A 200 has no reason at all, which is the point.
func answered(t *testing.T, h http.HandlerFunc, path, body string) (int, Reason) {
	t.Helper()
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body))))
	if w.Code == http.StatusOK {
		return w.Code, ""
	}
	var refused refusal
	if err := json.Unmarshal(w.Body.Bytes(), &refused); err != nil {
		t.Fatalf("decoding a %d: %v (%s)", w.Code, err, w.Body.String())
	}
	return w.Code, refused.Reason
}

func mustNeedRecovery(t *testing.T, what string, code int, reason Reason) {
	t.Helper()
	if code != http.StatusServiceUnavailable || reason != ReasonNeedsRecovery {
		t.Errorf("%s: got %d %q, want %d %q", what,
			code, reason, http.StatusServiceUnavailable, ReasonNeedsRecovery)
	}
}

// Scenario: a retry after a failed commit is not answered as success
//
//	Given a command whose commit failed, which stopped the session
//	When the client retries the identical command under the same gesture
//	Then it is refused with needs_recovery, and the store is untouched.
//
// The retry is the case that matters, because a client that cannot reach the
// server retries by design: that is what the gesture is for. The adapter
// recognises the retry from its own gesture register, which the failed command
// claimed on its way through, so the second attempt is answered "this is the
// act you already sent" — a 200 for a decision no store ever accepted.
//
// Two things are pinned, not one. The status is the first, and on its own it
// would only fix a number. The second is the one that protects the invariant:
// the batches the store holds are the same before the attempt and after the
// retry. No decision appeared out of nowhere.
func TestARetryAfterAFailedCommitIsRefusedRatherThanAnsweredWithState(t *testing.T) {
	s, committer, token, segment := onARunAboutToLoseItsJournal(t)
	run := market.FormatUint(segment)
	committed := len(committer.batches)

	committer.failing = true
	code, reason := answered(t, s.handleCommand, "/api/command", submitOrder(token, run+":1", "o-1"))
	mustNeedRecovery(t, "the command whose commit failed", code, reason)

	code, reason = answered(t, s.handleCommand, "/api/command", submitOrder(token, run+":1", "o-1"))
	mustNeedRecovery(t, "the retry of that same command", code, reason)

	if got := len(committer.batches); got != committed {
		t.Errorf("the store holds %d batches and held %d before the attempt: "+
			"a decision the disk never took is in it", got, committed)
	}
}

// Scenario: a name already spent by a failed commit is not a conflict
//
//	Given a command whose commit failed, which stopped the session
//	When a different command arrives under that same gesture
//	Then it is refused with needs_recovery, not gesture_conflict.
//
// This is the same defect from the other side of the register, and the reason
// it is worth its own scenario is the client. A 409 says "somebody got there
// first, look at the state and decide again"; a 503 needs_recovery says "stop,
// nothing here can be trusted until a person reads the journal". A client told
// the first will go on trading on a run that has stopped.
func TestAGestureSpentByAFailedCommitIsNotReportedAsAConflict(t *testing.T) {
	s, committer, token, segment := onARunAboutToLoseItsJournal(t)
	run := market.FormatUint(segment)

	committer.failing = true
	code, reason := answered(t, s.handleCommand, "/api/command", submitOrder(token, run+":1", "o-1"))
	mustNeedRecovery(t, "the command whose commit failed", code, reason)

	// A different order under the name the failed command spent.
	code, reason = answered(t, s.handleCommand, "/api/command", submitOrder(token, run+":1", "o-2"))
	mustNeedRecovery(t, "a different command under a spent gesture", code, reason)
}

// Scenario: a step whose commit failed is not reported as a stale tab
//
//	Given a step whose commit failed, which stopped the session
//	When the client retries that same step
//	Then it is refused with needs_recovery, not stale_step.
//
// The step's own bookkeeping produces this one. An observation records itself
// before the commit is attempted, so the row on the screen moves on in memory
// while the record of the last applied step does not — and the retry then looks
// exactly like a stale tab advancing from a row nobody stands on. It is not one.
// The run has stopped, and the client is told to reload instead of to recover.
func TestAStepWhoseCommitFailedIsNotReportedAsStale(t *testing.T) {
	s, committer, token, _ := onARunAboutToLoseItsJournal(t)
	from := market.FormatUint(s.session.LastObserved())

	committer.failing = true
	code, reason := answered(t, s.handleStep, "/api/step", stepFrom(token, from))
	mustNeedRecovery(t, "the step whose commit failed", code, reason)

	code, reason = answered(t, s.handleStep, "/api/step", stepFrom(token, from))
	mustNeedRecovery(t, "the retry of that same step", code, reason)
}

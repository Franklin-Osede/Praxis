// Package ui serves a local interface over one Praxis session.
//
// It is an adapter: it owns an HTTP server, a lease, and a projection of state
// designed for a protocol. No domain package imports it, and nothing in it
// reaches into a session except through the one loop that owns it.
//
// # Why the interface is experimental apparatus
//
// A screen looks like a layout decision and is not. What is on it is the
// participant's information set, how fast observations arrive is the pacing
// condition, and whether a duplicate request becomes a second decision is
// whether the journal holds acts the person never took. Section 11 of
// docs/PRAXIS_SPEC.md is the protocol this package implements.
package ui

import (
	"time"

	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"praxis/internal/session"
	"sync"
)

// Errors reported when control cannot be given or kept.
var (
	// ErrControllerActive reports a second client asking for control while
	// somebody has it. It is answered with 409: a second tab is a second hand
	// on the wheel, and the kernel's inputs must arrive in one order.
	ErrControllerActive = errors.New("ui: someone else is controlling this session")

	// ErrStaleLease reports a request from a lease that has been handed over
	// or replaced. It can arrive after the transfer — a slow request sent
	// before it — and is refused on arrival rather than by when it was sent.
	ErrStaleLease = errors.New("ui: this lease is no longer the controller")
)

// lease is who is driving, and which run of uninterrupted interaction their
// decisions belong to.
//
// The token is infrastructure and never enters the journal. The segment does,
// because it changes how the times in the journal are read — but the segment
// rule alone does not prove exclusivity: two tabs sharing one token could
// interleave their gestures inside a single segment without breaking it. That
// is what this type is for, and why it is tested here rather than inferred
// from the log.
type lease struct {
	mu      sync.Mutex
	token   string
	segment uint64

	// highest is the largest segment this journal has ever seen, so that a
	// restart continues above it rather than reusing a number whose decisions
	// are already recorded.
	highest uint64

	// startedMono is the monotonic reading the open segment began at, and it is
	// the only thing an elapsed is measured from. The wall reading is taken
	// fresh at every stamp and never subtracted: a clock correction moves it
	// and must not move an interval.
	//
	// Keeping only this one is what makes that testable. While the clock was a
	// func() time.Time the property was unreachable — a time.Time built by a
	// fixture carries no monotonic reading, so Sub fell back to the wall
	// difference and the two were indistinguishable under injection.
	startedMono time.Duration

	// now is the clock this lease stamps with. Injected so a test can produce
	// the same journal twice, and so that a wall clock going backwards while
	// the monotonic count goes forward is something a fixture can express.
	now Clock
}

// newLease starts with the highest segment the journal already holds. Nothing
// is granted yet: a server that has recovered a journal has not thereby given
// anyone the controls.
func newLease(highest uint64, now Clock) *lease {
	if now == nil {
		now = SystemClock()
	}
	return &lease{highest: highest, now: now}
}

// acquire gives control to a client that has none, and opens a new segment.
//
// Two clients asking at once produce one winner. The loser is refused rather
// than queued, because control is not something to wait for: the second tab
// should say so to its user immediately.
func (l *lease) acquire() (token string, segment uint64, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.token != "" {
		return "", 0, ErrControllerActive
	}
	return l.grant()
}

// transfer takes control from whoever has it. It is explicit, because control
// changing hands silently is how two people end up believing they are driving.
func (l *lease) transfer() (token string, segment uint64, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.grant()
}

// grant mints a lease. The caller holds the mutex.
func (l *lease) grant() (string, uint64, error) {
	// Infrastructure randomness, and it never reaches the kernel: nothing in a
	// journal depends on this value, so rule 3's requirement that randomness
	// be injected and its seed recorded is about a different thing entirely.
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", 0, fmt.Errorf("ui: cannot mint a lease: %w", err)
	}
	l.token = hex.EncodeToString(raw)
	l.highest++
	l.segment = l.highest
	// A new run of interaction begins here, so its readings start here too.
	l.startedMono = l.now().Mono
	return l.token, l.segment, nil
}

// check refuses a request that does not hold the current lease, and reports the
// segment its decisions belong to.
//
// Reconnecting on the same token keeps the segment: losing the view is not
// losing the controls, and a new segment would say a run of interaction ended
// when only a socket did.
// stamp is the moment a request arrived, in the run the token holds. It is the
// server's reading and not the client's: the interval a hypothesis measures runs
// from server receipt to server receipt, and a figure the browser supplied would
// be the one thing in this journal that nothing can contradict.
func (l *lease) stamp(token string) (session.Instant, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.token == "" || token != l.token {
		return session.Instant{}, ErrStaleLease
	}
	at := l.now()
	return session.Instant{
		AtUTCNanos:   session.UnixNanos(at.Wall.UnixNano()),
		Segment:      l.segment,
		ElapsedNanos: session.ElapsedNanos((at.Mono - l.startedMono).Nanoseconds()),
	}, nil
}

func (l *lease) check(token string) (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.token == "" {
		return 0, ErrStaleLease
	}
	if token != l.token {
		return 0, ErrStaleLease
	}
	return l.segment, nil
}

// release gives up control without opening anything. A lease taken and
// abandoned with no events produced leaves nothing in the journal, which is
// correct: no decisions were made in it, so there is no segment to record.
func (l *lease) release(token string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if token != l.token || l.token == "" {
		return ErrStaleLease
	}
	l.token, l.segment = "", 0
	return nil
}

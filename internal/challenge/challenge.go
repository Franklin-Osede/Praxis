// Package challenge applies evaluation rules to an account's equity and
// decides consequences. It knows nothing about any particular firm: a provider
// is a set of values in Rules, never a branch.
//
// It also knows nothing about clocks or calendars. A trading day arrives as a
// SessionID stamped by an adapter, and a session begins with an explicit
// SessionOpened. See docs/adr/011-session-boundaries.md.
package challenge

import (
	"errors"
	"fmt"

	"praxis/internal/market"
)

// SessionID identifies a trading session. It is compared for equality and
// never parsed, ordered or read as a date, so an adapter may use any stable
// scheme.
//
// It lives here because the challenge engine is its only consumer. When market
// observations start carrying it, it belongs in the market package.
type SessionID string

// State is the challenge's position in its lifecycle.
type State uint8

const (
	StatePending State = iota
	StateActive
	StatePassed
	StateFailed
)

func (s State) String() string {
	switch s {
	case StateActive:
		return "active"
	case StatePassed:
		return "passed"
	case StateFailed:
		return "failed"
	default:
		return "pending"
	}
}

// FailureReason names the rule that ended an evaluation.
type FailureReason uint8

const (
	FailureNone FailureReason = iota
	FailureDailyLoss
)

func (r FailureReason) String() string {
	if r == FailureDailyLoss {
		return "daily loss limit"
	}
	return "none"
}

// Rules are an evaluation's configuration. One rule exists so far.
type Rules struct {
	// MaxDailyLossCts is the largest loss tolerated within one session,
	// measured against that session's reference equity. Losing exactly this
	// much does not fail; losing more does.
	MaxDailyLossCts market.Cents
}

// Validate reports why the rules are not a valid domain value, or nil.
func (r Rules) Validate() error {
	if r.MaxDailyLossCts <= 0 {
		return ErrNonPositiveDailyLoss
	}
	return nil
}

// SessionOpened asserts that a trading session has begun, and states the
// equity the session's rules will be measured against.
type SessionOpened struct {
	Time               market.LogicalTime
	Sequence           uint64
	SessionID          SessionID
	ReferenceEquityCts market.Cents
}

// AccountSnapshot is an account's equity at a point in an open session.
type AccountSnapshot struct {
	Time      market.LogicalTime
	Sequence  uint64
	SessionID SessionID
	EquityCts market.Cents
}

// EventKind names what the challenge engine decided.
type EventKind uint8

const (
	SessionStarted EventKind = iota + 1
	ChallengeActivated
	ChallengeFailed
)

func (k EventKind) String() string {
	switch k {
	case SessionStarted:
		return "session started"
	case ChallengeActivated:
		return "challenge activated"
	case ChallengeFailed:
		return "challenge failed"
	default:
		return "unspecified"
	}
}

// Event is a decision the challenge engine made, at the position in the input
// stream that caused it.
type Event struct {
	Kind      EventKind
	Time      market.LogicalTime
	Sequence  uint64
	SessionID SessionID

	// EquityCts is the session's reference on SessionStarted, and the observed
	// equity on ChallengeFailed.
	EquityCts market.Cents

	// LossCts and Reason are set on ChallengeFailed only.
	LossCts market.Cents
	Reason  FailureReason
}

// Errors reported for input that cannot describe a real evaluation.
var (
	ErrNonPositiveDailyLoss = errors.New("challenge: daily loss limit is not positive")
	ErrEmptySessionID       = errors.New("challenge: session id is empty")
	ErrOutOfOrder           = errors.New("challenge: input is not after the last accepted one")
	ErrSessionReturned      = errors.New("challenge: session id has already been used")
	ErrUnknownSession       = errors.New("challenge: snapshot for a session that was never opened")
	ErrNotActive            = errors.New("challenge: no session is open")
	ErrTerminal             = errors.New("challenge: evaluation has already ended")
)

// Challenge is an evaluation in progress.
type Challenge struct {
	rules Rules

	state   State
	failure FailureReason

	currentSessionID SessionID
	referenceCts     market.Cents

	// seenSessions is an ordered slice and not a map: a session identifier
	// never returns, and how that is checked must not depend on iteration
	// order.
	seenSessions []SessionID

	lastTime     market.LogicalTime
	lastSequence uint64
	started      bool
}

// New builds a pending challenge.
func New(rules Rules) (*Challenge, error) {
	if err := rules.Validate(); err != nil {
		return nil, err
	}
	return &Challenge{rules: rules, state: StatePending}, nil
}

func (c *Challenge) State() State                 { return c.state }
func (c *Challenge) FailureReason() FailureReason { return c.failure }
func (c *Challenge) SessionID() SessionID         { return c.currentSessionID }
func (c *Challenge) ReferenceEquityCts() market.Cents {
	return c.referenceCts
}

// OpenSession begins a trading session, activating a pending challenge.
//
// A session is never inferred from an ordinary snapshot. The adapter asserts
// the boundary and states the equity the session will be measured against, so
// a lost first snapshot cannot silently re-base the reference against a later,
// different equity.
func (c *Challenge) OpenSession(o SessionOpened) ([]Event, error) {
	if c.ended() {
		return nil, fmt.Errorf("%w: %s", ErrTerminal, c.state)
	}
	if o.SessionID == "" {
		return nil, ErrEmptySessionID
	}
	if err := c.checkOrder(o.Time, o.Sequence); err != nil {
		return nil, err
	}
	if c.hasSeen(o.SessionID) {
		return nil, fmt.Errorf("%w: %s", ErrSessionReturned, o.SessionID)
	}

	var events []Event
	if c.state == StatePending {
		c.state = StateActive
		events = append(events, Event{
			Kind: ChallengeActivated, Time: o.Time, Sequence: o.Sequence,
			SessionID: o.SessionID, EquityCts: o.ReferenceEquityCts,
		})
	}
	events = append(events, Event{
		Kind: SessionStarted, Time: o.Time, Sequence: o.Sequence,
		SessionID: o.SessionID, EquityCts: o.ReferenceEquityCts,
	})

	c.currentSessionID = o.SessionID
	c.referenceCts = o.ReferenceEquityCts
	c.seenSessions = append(c.seenSessions, o.SessionID)
	c.accept(o.Time, o.Sequence)
	return events, nil
}

// Observe applies the rules to an account's equity within the open session.
//
// The loss is measured against the session's reference, so time passing—even
// past a calendar day—changes nothing. Only a new session re-bases it. Losing
// exactly the limit does not fail; losing more does.
func (c *Challenge) Observe(snapshot AccountSnapshot) ([]Event, error) {
	if c.ended() {
		return nil, fmt.Errorf("%w: %s", ErrTerminal, c.state)
	}
	if c.state != StateActive {
		return nil, ErrNotActive
	}
	if err := c.checkOrder(snapshot.Time, snapshot.Sequence); err != nil {
		return nil, err
	}
	if snapshot.SessionID != c.currentSessionID {
		return nil, fmt.Errorf("%w: %s, open session is %s",
			ErrUnknownSession, snapshot.SessionID, c.currentSessionID)
	}

	lossCts, err := market.SubCents(c.referenceCts, snapshot.EquityCts)
	if err != nil {
		return nil, err
	}

	var events []Event
	if lossCts > c.rules.MaxDailyLossCts {
		c.state = StateFailed
		c.failure = FailureDailyLoss
		events = append(events, Event{
			Kind: ChallengeFailed, Time: snapshot.Time, Sequence: snapshot.Sequence,
			SessionID: snapshot.SessionID, EquityCts: snapshot.EquityCts,
			LossCts: lossCts, Reason: FailureDailyLoss,
		})
	}

	c.accept(snapshot.Time, snapshot.Sequence)
	return events, nil
}

func (c *Challenge) ended() bool {
	return c.state == StateFailed || c.state == StatePassed
}

// checkOrder enforces one strictly increasing order over both inputs. An
// out-of-order input is rejected rather than sorted, because sorting would
// hide an adapter defect that changes results.
func (c *Challenge) checkOrder(t market.LogicalTime, seq uint64) error {
	if !c.started {
		return nil
	}
	if t > c.lastTime || (t == c.lastTime && seq > c.lastSequence) {
		return nil
	}
	return fmt.Errorf("%w: (%d, %d) after (%d, %d)", ErrOutOfOrder, t, seq, c.lastTime, c.lastSequence)
}

func (c *Challenge) accept(t market.LogicalTime, seq uint64) {
	c.lastTime, c.lastSequence, c.started = t, seq, true
}

// hasSeen reports whether a session identifier has already been used. The scan
// is over an ordered slice so that it cannot depend on iteration order.
func (c *Challenge) hasSeen(id SessionID) bool {
	for _, seen := range c.seenSessions {
		if seen == id {
			return true
		}
	}
	return false
}

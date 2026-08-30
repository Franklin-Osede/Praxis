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

	// ProfitTargetCts is the gain that passes the evaluation, measured
	// against the equity the evaluation began with and never against a
	// session's reference. Reaching it exactly is enough. Zero means no
	// target is configured and the evaluation can only be failed.
	ProfitTargetCts market.Cents
}

// Validate reports why the rules are not a valid domain value, or nil.
func (r Rules) Validate() error {
	if r.MaxDailyLossCts <= 0 {
		return ErrNonPositiveDailyLoss
	}
	if r.ProfitTargetCts < 0 {
		return ErrNegativeProfitTarget
	}
	return nil
}

// SessionOpened asserts that a trading session has begun, and states the
// account's valuation at the boundary.
type SessionOpened struct {
	Time      market.LogicalTime
	Sequence  uint64
	SessionID SessionID

	// BalanceCts and EquityCts carry the same valuation an AccountSnapshot
	// does, for the same reason: a rule that reads one must never be handed
	// the other. The session's loss reference comes from EquityCts, and the
	// evaluation's target base is the BalanceCts of the session that
	// activated it.
	BalanceCts market.Cents
	EquityCts  market.Cents
}

// AccountSnapshot is an account's valuation at a point in an open session.
//
// Balance and equity are separate because the rules disagree about which one
// they mean. An open loss must be able to fail an evaluation immediately, so
// the loss rules read equity. An open gain must not pass one, because a
// position that touches the target for an instant and gives it all back would
// otherwise have bought an irreversible approval — so the target reads
// balance, and only realised money counts toward it.
//
// Challenge infers neither value. Both arrive from one atomic valuation of the
// account, and nothing here can check that they were taken together.
type AccountSnapshot struct {
	Time      market.LogicalTime
	Sequence  uint64
	SessionID SessionID

	// BalanceCts is the starting balance plus realised P&L, minus fees.
	BalanceCts market.Cents

	// EquityCts is the balance plus unrealised P&L.
	EquityCts market.Cents
}

// EventKind names what the challenge engine decided.
type EventKind uint8

const (
	ChallengeActivated EventKind = iota + 1

	// SessionReferenceEstablished is not the start of a session. Challenge
	// does not start sessions; it reacts to a SessionOpened asserted by an
	// adapter. The name SessionStarted is reserved for the global session
	// event that carries provenance and the random seed, and both will be
	// serialised into one behavioural log.
	SessionReferenceEstablished

	ChallengeFailed
	ChallengePassed
)

func (k EventKind) String() string {
	switch k {
	case ChallengeActivated:
		return "challenge activated"
	case SessionReferenceEstablished:
		return "session reference established"
	case ChallengeFailed:
		return "challenge failed"
	case ChallengePassed:
		return "challenge passed"
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

	// EquityCts is the session's equity reference on
	// SessionReferenceEstablished, the observed equity on ChallengeFailed, and
	// the observed balance on ChallengePassed — each rule reports the value it
	// was decided on.
	EquityCts market.Cents

	// LossCts and Reason are set on ChallengeFailed only.
	LossCts market.Cents
	Reason  FailureReason

	// GainCts is set on ChallengePassed only, measured on balance from the
	// balance the evaluation began with.
	GainCts market.Cents
}

// Errors reported for input that cannot describe a real evaluation.
var (
	ErrNonPositiveDailyLoss = errors.New("challenge: daily loss limit is not positive")
	ErrNegativeProfitTarget = errors.New("challenge: profit target is negative")
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

	// startingBalanceCts is the balance the evaluation began with and is never
	// re-based; referenceCts is the open session's equity reference and
	// re-bases at every boundary. The profit target is measured against the
	// first, the daily loss limit against the second.
	startingBalanceCts market.Cents
	referenceCts       market.Cents

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

func (c *Challenge) State() State                     { return c.state }
func (c *Challenge) FailureReason() FailureReason     { return c.failure }
func (c *Challenge) SessionID() SessionID             { return c.currentSessionID }
func (c *Challenge) ReferenceEquityCts() market.Cents { return c.referenceCts }
func (c *Challenge) StartingBalanceCts() market.Cents { return c.startingBalanceCts }

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
		c.startingBalanceCts = o.BalanceCts
		events = append(events, Event{
			Kind: ChallengeActivated, Time: o.Time, Sequence: o.Sequence,
			SessionID: o.SessionID, EquityCts: o.EquityCts,
		})
	}
	events = append(events, Event{
		Kind: SessionReferenceEstablished, Time: o.Time, Sequence: o.Sequence,
		SessionID: o.SessionID, EquityCts: o.EquityCts,
	})

	c.currentSessionID = o.SessionID
	c.referenceCts = o.EquityCts
	c.seenSessions = append(c.seenSessions, o.SessionID)
	c.accept(o.Time, o.Sequence)
	return events, nil
}

// Observe applies the rules to an account's equity within the open session.
//
// The loss is measured against the session's reference, so time passing—even
// past a calendar day—changes nothing. Only a new session re-bases it. Losing
// exactly the limit does not fail; losing more does.
//
// The profit target is measured on balance, against the balance the evaluation
// began with, which no boundary moves. Reaching it exactly is enough. It reads
// balance and not equity because an open gain that is given back must not have
// bought an irreversible approval: money passes an evaluation once it has been
// realised, not while it is still on the screen. Losses are the opposite — an
// open one can end an evaluation immediately.
//
// Both can breach on the same snapshot, because they are measured against
// different references: a session that opened after a large run-up can be far
// enough down on the day to fail while the evaluation is still far enough up
// to pass. The rules are coherent and the state is reachable, so the
// precedence is decided rather than left open, and the loss wins. A simulator
// must never resolve an ambiguity in the trader's favour.
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

	gainCts, err := market.SubCents(snapshot.BalanceCts, c.startingBalanceCts)
	if err != nil {
		return nil, err
	}

	var events []Event
	switch {
	case lossCts > c.rules.MaxDailyLossCts:
		c.state = StateFailed
		c.failure = FailureDailyLoss
		events = append(events, Event{
			Kind: ChallengeFailed, Time: snapshot.Time, Sequence: snapshot.Sequence,
			SessionID: snapshot.SessionID, EquityCts: snapshot.EquityCts,
			LossCts: lossCts, Reason: FailureDailyLoss,
		})
	case c.rules.ProfitTargetCts > 0 && gainCts >= c.rules.ProfitTargetCts:
		c.state = StatePassed
		events = append(events, Event{
			Kind: ChallengePassed, Time: snapshot.Time, Sequence: snapshot.Sequence,
			SessionID: snapshot.SessionID, EquityCts: snapshot.BalanceCts,
			GainCts: gainCts,
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

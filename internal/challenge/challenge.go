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
	FailureStaticDrawdown
	FailureTrailingDrawdown
)

func (r FailureReason) String() string {
	switch r {
	case FailureDailyLoss:
		return "daily loss limit"
	case FailureStaticDrawdown:
		return "static drawdown"
	case FailureTrailingDrawdown:
		return "trailing drawdown"
	default:
		return "none"
	}
}

// Rules are an evaluation's configuration.
type Rules struct {
	// StartingBalanceCts is the valuation the evaluation is contracted to
	// begin at. The static drawdown floor and the profit target are both
	// anchored to it, so neither depends on a mark taken at the instant of
	// activation.
	StartingBalanceCts market.Cents

	// MaxDailyLossCts is the largest loss tolerated within one session,
	// measured against that session's reference equity. Losing exactly this
	// much does not fail; losing more does.
	MaxDailyLossCts market.Cents

	// ProfitTargetCts is the gain that passes the evaluation, measured
	// against the configured starting balance and never against a
	// session's reference. Reaching it exactly is enough. Zero means no
	// target is configured and the evaluation can only be failed.
	ProfitTargetCts market.Cents

	// MaxTotalLossCts is the largest fall below the starting balance the
	// evaluation tolerates, measured on equity and never re-based by a
	// session or lifted by previous profit. Zero means no static floor.
	MaxTotalLossCts market.Cents

	// TrailingDrawdownCts is the permitted fall from the highest equity seen
	// by the evaluation. The high-water mark includes unrealised P&L, never
	// falls or resets at a session boundary, and trails until the evaluation
	// ends. Zero disables the rule.
	TrailingDrawdownCts market.Cents
}

// staticFloorCts derives the configured static boundary. Its caller keeps
// enablement separate because zero is also a valid boundary.
func (r Rules) staticFloorCts() (market.Cents, error) {
	if r.MaxTotalLossCts == 0 {
		return 0, nil
	}
	return market.SubCents(r.StartingBalanceCts, r.MaxTotalLossCts)
}

// Validate reports why the rules are not a valid domain value, or nil.
func (r Rules) Validate() error {
	if r.StartingBalanceCts <= 0 {
		return ErrNonPositiveStartingBalance
	}
	if r.MaxDailyLossCts <= 0 {
		return ErrNonPositiveDailyLoss
	}
	if r.ProfitTargetCts < 0 {
		return ErrNegativeProfitTarget
	}
	if r.MaxTotalLossCts < 0 {
		return ErrNegativeTotalLoss
	}
	if r.TrailingDrawdownCts < 0 {
		return ErrNegativeTrailingDrawdown
	}
	if _, err := r.staticFloorCts(); err != nil {
		return err
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

// Decision is what the challenge engine decided, with no position in any
// stream. It is kept separate from Event so that a log recording a decision
// carries one sequence — its own — rather than two fields with the same name
// meaning different things.
type Decision struct {
	Kind      EventKind
	SessionID SessionID

	// BalanceCts and EquityCts carry the whole valuation that produced the
	// decision, never one figure whose meaning depends on Kind. A persisted
	// log must not have to reinterpret a field to know what it holds.
	BalanceCts market.Cents
	EquityCts  market.Cents

	// LossCts and Reason are set on ChallengeFailed only. The loss was
	// measured on EquityCts.
	LossCts market.Cents
	Reason  FailureReason

	// GainCts is set on ChallengePassed only. It was measured on BalanceCts,
	// against the balance the evaluation began with.
	GainCts market.Cents

	// HighWaterCts and ThresholdCts are set on a trailing-drawdown failure.
	// They make the precise moving boundary that caused the decision
	// recoverable from the event log.
	HighWaterCts market.Cents
	ThresholdCts market.Cents
}

// Event is a Decision together with the position of the input that caused it.
type Event struct {
	Time     market.LogicalTime
	Sequence uint64
	Decision
}

// Errors reported for input that cannot describe a real evaluation.
var (
	ErrNonPositiveStartingBalance = errors.New("challenge: starting balance is not positive")
	ErrNonPositiveDailyLoss       = errors.New("challenge: daily loss limit is not positive")
	ErrNegativeProfitTarget       = errors.New("challenge: profit target is negative")
	ErrNegativeTotalLoss          = errors.New("challenge: maximum total loss is negative")
	ErrNegativeTrailingDrawdown   = errors.New("challenge: trailing drawdown is negative")

	// ErrNotStartingValuation guards the activating valuation. It does not
	// prove the account holds no position — a position at break-even has zero
	// unrealised P&L and would satisfy it too. It proves only that the
	// evaluation began at the valuation it was contracted to begin at. The
	// session layer must open a challenge immediately after creating the
	// account and before accepting any order.
	ErrNotStartingValuation = errors.New("challenge: does not activate at its configured starting valuation")
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

	// staticFloorCts is derived from the rules once and never moves.
	// referenceCts is the open session's equity reference and re-bases at
	// every boundary.
	staticFloorCts market.Cents
	referenceCts   market.Cents

	// highWaterCts and trailingThresholdCts exist only when the trailing
	// rule is configured. They are committed after an input has been fully
	// validated and all checked arithmetic succeeds.
	highWaterCts         market.Cents
	trailingThresholdCts market.Cents

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
	floor, err := rules.staticFloorCts()
	if err != nil {
		return nil, err
	}
	c := &Challenge{rules: rules, state: StatePending, staticFloorCts: floor}
	if rules.TrailingDrawdownCts > 0 {
		threshold, err := market.SubCents(rules.StartingBalanceCts, rules.TrailingDrawdownCts)
		if err != nil {
			return nil, err
		}
		c.highWaterCts = rules.StartingBalanceCts
		c.trailingThresholdCts = threshold
	}
	return c, nil
}

func (c *Challenge) State() State                     { return c.state }
func (c *Challenge) FailureReason() FailureReason     { return c.failure }
func (c *Challenge) SessionID() SessionID             { return c.currentSessionID }
func (c *Challenge) ReferenceEquityCts() market.Cents { return c.referenceCts }

// StaticFloor returns the configured static floor and whether the rule is
// enabled. The boolean is necessary because zero is both a valid floor and
// the zero value of Cents.
func (c *Challenge) StaticFloor() (market.Cents, bool) {
	return c.staticFloorCts, c.rules.MaxTotalLossCts > 0
}

// HighWater returns the highest equity observed and whether trailing drawdown
// is enabled. A monetary zero is never used to mean "not configured".
func (c *Challenge) HighWater() (market.Cents, bool) {
	return c.highWaterCts, c.rules.TrailingDrawdownCts > 0
}

// TrailingThresholdCts returns the current trailing floor and whether the
// rule is enabled. A threshold of zero is not used as an absence sentinel.
func (c *Challenge) TrailingThresholdCts() (market.Cents, bool) {
	return c.trailingThresholdCts, c.rules.TrailingDrawdownCts > 0
}
func (c *Challenge) StartingBalanceCts() market.Cents {
	return c.rules.StartingBalanceCts
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

	if c.state == StatePending &&
		(o.BalanceCts != c.rules.StartingBalanceCts || o.EquityCts != c.rules.StartingBalanceCts) {
		return nil, fmt.Errorf("%w: opened at balance %d equity %d, configured %d",
			ErrNotStartingValuation, o.BalanceCts, o.EquityCts, c.rules.StartingBalanceCts)
	}

	var events []Event
	if c.state == StatePending {
		c.state = StateActive
		events = append(events, Event{
			Time: o.Time, Sequence: o.Sequence,
			Decision: Decision{
				Kind: ChallengeActivated, SessionID: o.SessionID,
				BalanceCts: o.BalanceCts, EquityCts: o.EquityCts,
			},
		})
	}
	events = append(events, Event{
		Time: o.Time, Sequence: o.Sequence,
		Decision: Decision{
			Kind: SessionReferenceEstablished, SessionID: o.SessionID,
			BalanceCts: o.BalanceCts, EquityCts: o.EquityCts,
		},
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
//
// The static drawdown floor is anchored to the configured starting balance, so
// no session re-bases it and no earlier profit lifts it. Equity exactly at the
// floor is still active; below it is not. When it breaches together with the
// daily limit, the daily limit is reported: the outcome is identical and
// keeping the older rule first leaves recorded reasons stable.
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

	gainCts, err := market.SubCents(snapshot.BalanceCts, c.rules.StartingBalanceCts)
	if err != nil {
		return nil, err
	}

	// Derive the complete trailing candidate before deciding or mutating.
	// The current observation may establish a new high; the same equity is
	// then evaluated against the threshold derived from that candidate.
	candidateHighWaterCts := c.highWaterCts
	candidateThresholdCts := c.trailingThresholdCts
	var trailingLossCts market.Cents
	if c.rules.TrailingDrawdownCts > 0 {
		if snapshot.EquityCts > candidateHighWaterCts {
			candidateHighWaterCts = snapshot.EquityCts
		}
		candidateThresholdCts, err = market.SubCents(candidateHighWaterCts, c.rules.TrailingDrawdownCts)
		if err != nil {
			return nil, err
		}
		trailingLossCts, err = market.SubCents(candidateHighWaterCts, snapshot.EquityCts)
		if err != nil {
			return nil, err
		}
	}

	fail := func(reason FailureReason, amountCts market.Cents) []Event {
		c.state = StateFailed
		c.failure = reason
		return []Event{{
			Time: snapshot.Time, Sequence: snapshot.Sequence,
			Decision: Decision{
				Kind: ChallengeFailed, SessionID: snapshot.SessionID,
				BalanceCts: snapshot.BalanceCts, EquityCts: snapshot.EquityCts,
				LossCts: amountCts, Reason: reason,
			},
		}}
	}

	var events []Event
	switch {
	case lossCts > c.rules.MaxDailyLossCts:
		events = fail(FailureDailyLoss, lossCts)
	case c.rules.MaxTotalLossCts > 0 && snapshot.EquityCts < c.staticFloorCts:
		totalLossCts, err := market.SubCents(c.rules.StartingBalanceCts, snapshot.EquityCts)
		if err != nil {
			return nil, err
		}
		events = fail(FailureStaticDrawdown, totalLossCts)
	case c.rules.TrailingDrawdownCts > 0 && snapshot.EquityCts < candidateThresholdCts:
		events = fail(FailureTrailingDrawdown, trailingLossCts)
		events[0].HighWaterCts = candidateHighWaterCts
		events[0].ThresholdCts = candidateThresholdCts
	case c.rules.ProfitTargetCts > 0 && gainCts >= c.rules.ProfitTargetCts:
		c.state = StatePassed
		events = append(events, Event{
			Time: snapshot.Time, Sequence: snapshot.Sequence,
			Decision: Decision{
				Kind: ChallengePassed, SessionID: snapshot.SessionID,
				BalanceCts: snapshot.BalanceCts, EquityCts: snapshot.EquityCts,
				GainCts: gainCts,
			},
		})
	}

	if c.rules.TrailingDrawdownCts > 0 {
		c.highWaterCts = candidateHighWaterCts
		c.trailingThresholdCts = candidateThresholdCts
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

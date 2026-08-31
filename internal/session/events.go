// Package session orchestrates one deterministic trading session: market
// observations reach execution, fills reach the account, the account's
// valuation reaches the challenge engine, and every resulting fact is appended
// to an ordered journal.
//
// It composes the domain packages and owns none of their rules. It is also the
// only place that knows both a position's direction and the current book, so
// it is where a position is marked.
package session

import (
	"praxis/internal/challenge"
	"praxis/internal/market"
	"praxis/internal/portfolio"
)

// Kind tags an event so a serialised log can be read without inferring a type.
type Kind uint8

const (
	KindSessionStarted Kind = iota + 1
	KindSessionOpened
	KindMarketObserved
	KindOrderSubmitted
	KindFillProduced
	KindPositionChanged
	KindAccountValued
	KindChallengeDecision
	KindSessionEnded
)

func (k Kind) String() string {
	switch k {
	case KindSessionStarted:
		return "session started"
	case KindSessionOpened:
		return "session opened"
	case KindMarketObserved:
		return "market observed"
	case KindOrderSubmitted:
		return "order submitted"
	case KindFillProduced:
		return "fill produced"
	case KindPositionChanged:
		return "position changed"
	case KindAccountValued:
		return "account valued"
	case KindChallengeDecision:
		return "challenge decision"
	case KindSessionEnded:
		return "session ended"
	default:
		return "unspecified"
	}
}

// Envelope is the position of an event in the one ordered stream. Every event
// embeds it, which is also what makes the Event interface sealed to this
// package.
type Envelope struct {
	Time     market.LogicalTime
	Sequence uint64
	Kind     Kind
}

func (e Envelope) Header() Envelope { return e }
func (e Envelope) isEvent()         {}

// Event is a fact the session recorded. The set is closed: a payload is a
// typed field on a concrete event, never a map, both because a map's iteration
// order must never reach a serialiser and because an untyped payload cannot be
// read back without guessing.
type Event interface {
	Header() Envelope
	isEvent()
}

// Config is everything needed to reproduce a session from its log.
type Config struct {
	Instrument               market.Instrument
	StartingBalanceCts       market.Cents
	CommissionPerContractCts market.Cents
	Rules                    challenge.Rules
}

// SessionStarted opens the log. It carries the configuration, and nothing else
// in the log may depend on state that is not derivable from it.
//
// It records no random seed. Nothing in a scripted or replayed session is
// random, and a seed field that no consumer reads would be less truthful than
// its absence. When randomness first enters, an injected Randomizer and its
// explicit seed become required together and are recorded here.
type SessionStarted struct {
	Envelope
	Config Config
}

// SessionOpened is a trading session boundary asserted by the orchestrator,
// carrying the valuation the challenge engine will measure against.
type SessionOpened struct {
	Envelope
	SessionID  challenge.SessionID
	BalanceCts market.Cents
	EquityCts  market.Cents
}

// MarketObserved is an accepted observation of the book.
type MarketObserved struct {
	Envelope
	Quote market.Quote
}

// OrderContext is what the account looked like at the instant an order was
// submitted. Every field is a fact the system knows at that moment; none is an
// interpretation. Naming a pattern — revenge trading, overtrading — is
// analytics, and belongs nowhere near this package.
//
// Each field is also derivable from the events that precede it, and the replay
// test proves that it agrees with them. A log that can contradict itself is
// worse than one that stores less.
type OrderContext struct {
	BalanceCts market.Cents
	EquityCts  market.Cents

	// TradesThisSession counts the orders submitted in this trading session
	// before this one.
	TradesThisSession uint32

	// ConsecutiveLosses counts the closing legs that realised a loss in an
	// unbroken run immediately before this order, within this session.
	ConsecutiveLosses uint32

	// SessionRealisedCts is the P&L realised so far in this trading session.
	SessionRealisedCts market.Cents

	// PositionQtyBefore is the net position this order was submitted into.
	PositionQtyBefore market.Qty
}

// OrderSubmitted is a decision. It is the reason Praxis exists, so it records
// the state the decision was taken in, not only the instruction.
type OrderSubmitted struct {
	Envelope
	Order   market.Order
	Context OrderContext
}

// FillProduced is an execution fact.
type FillProduced struct {
	Envelope
	Fill market.Fill
}

// PositionChanged is one leg of what a fill did to the account.
type PositionChanged struct {
	Envelope
	Change portfolio.PositionEvent
}

// AccountValued is one atomic valuation of the account: both figures taken
// together, at the same marks.
type AccountValued struct {
	Envelope
	SessionID  challenge.SessionID
	BalanceCts market.Cents
	EquityCts  market.Cents
}

// ChallengeDecision wraps what the challenge engine decided. The inner event
// keeps its own vocabulary rather than being flattened into four session event
// types, so there is one definition of a challenge decision.
type ChallengeDecision struct {
	Envelope
	Decision challenge.Event
}

// SessionEnded closes a trading session. Reaching the end of a stream is not a
// boundary; ending one is an assertion, as opening one is.
type SessionEnded struct {
	Envelope
	SessionID challenge.SessionID
}

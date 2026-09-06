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
	KindOrderRested
	KindOrderCancelled
	KindProtectionPlaced
	KindProtectionReplaced
	KindProtectionEnded
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
	case KindOrderRested:
		return "order rested"
	case KindOrderCancelled:
		return "order cancelled"
	case KindProtectionPlaced:
		return "protection placed"
	case KindProtectionReplaced:
		return "protection replaced"
	case KindProtectionEnded:
		return "protection ended"
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

	// SubjectID is who traded this journal, as a label the protocol assigns —
	// never a name, an email or anything else that identifies a person. It is
	// empty for a journal nobody traded: a scripted run, a test, a replay.
	//
	// It is recorded because the experiment's unit of analysis is a trader and
	// not a session. Sessions from one person are a cluster, most of the
	// variance in a behavioural trait lives between people rather than within
	// them, and an analysis that could not tell one trader's sessions from
	// another's would be computing a number about nobody. Deriving it from a
	// file name afterwards is not evidence, and by this repository's own
	// version rule adding it after the first recorded session would be a new
	// payload version with a migration behind it. It costs nothing today.
	SubjectID string
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

	// SourceSequence is the position the source gave this observation, which
	// with its logical time is what orders it (ADR-010). Two observations at
	// the same instant differ only in this, so without it a journal could not
	// tell one from the other, and a source file whose same-time rows had been
	// swapped would verify against a journal that did not describe it.
	SourceSequence uint64
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

	// OrdersSubmittedThisSession counts the orders submitted in this trading
	// session before this one. It counts orders, not trades and not fills:
	// an order may not execute, may fill partially, or may later be
	// cancelled, and conflating the three would misreport behaviour as soon
	// as any of those exist.
	OrdersSubmittedThisSession uint32

	// ConsecutiveLosses counts the closing legs that realised a loss in an
	// unbroken run immediately before this order, within this session.
	ConsecutiveLosses uint32

	// SessionRealisedCts is the P&L realised so far in this trading session.
	SessionRealisedCts market.Cents

	// PositionQtyBefore is the net position this order was submitted into.
	PositionQtyBefore market.Qty

	// ConsecutiveLosingTrades counts completed position episodes that ended at
	// a loss, in an unbroken run, as it stood when this order was submitted.
	//
	// It is not ConsecutiveLosses, which counts closing legs: scaling out of
	// one bad position in two reductions is one trade abandoned in pieces, not
	// a streak. Both are facts about different things and both are recorded.
	// See ADR-013.
	ConsecutiveLosingTrades uint32
}

// OrderSubmitted is a decision. It is the reason Praxis exists, so it records
// the state the decision was taken in, not only the instruction.
type OrderSubmitted struct {
	Envelope
	Order   market.Order
	Context OrderContext

	// DecidedAt is when the person sent it, by their own clock. The envelope's
	// time is the market's, and two orders between one tick and the next carry
	// the same one — so the interval between decisions, which is what a
	// hypothesis about hesitation measures, is not in the envelope. Zero means
	// no person was there.
	DecidedAt market.WallClock
}

// CancelReason says why an order stopped working. It is a fact about what the
// system did, not a judgement about why the trader did anything.
type CancelReason uint8

const (
	// CancelledByTrader: the trader asked for it.
	CancelledByTrader CancelReason = iota + 1

	// CancelledUnfillableRemainder: the part of a market order the book could
	// not fill, and what is left of a stop that has triggered. A market order
	// does not rest, because resting one would mean inventing a price the
	// trader never named, and a triggered stop cannot untrigger.
	CancelledUnfillableRemainder

	// CancelledByOCO: the sibling of a protective leg that executed. One leg
	// closing the position leaves the other with nothing to close.
	CancelledByOCO

	// CancelledPositionClosed: a protective leg whose position went away
	// without it — a manual exit, or a reversal. The observable cause differs
	// from OCO even though both ends are the same, and a log that spelled them
	// alike could not tell a stop that worked from one that was overtaken.
	CancelledPositionClosed
)

func (r CancelReason) String() string {
	switch r {
	case CancelledByTrader:
		return "by trader"
	case CancelledUnfillableRemainder:
		return "unfillable remainder"
	case CancelledByOCO:
		return "by one-cancels-the-other"
	case CancelledPositionClosed:
		return "position closed"
	default:
		return "unspecified"
	}
}

// OrderRested records that an order, or what is left of one, is now waiting
// for a later observation.
//
// A stop is only a stop because it survives until the market reaches it. Until
// this event existed, an order that did not fill against the observation it was
// submitted on was dropped, and the log said nothing about it — so "how often
// does the trader re-enter after a partial fill" would have measured an
// artefact of the simulator rather than the trader.
type OrderRested struct {
	Envelope
	Order market.Order

	// RestingQty is what remains working, which is the whole order when
	// nothing filled and the remainder when something did.
	RestingQty market.Qty
}

// OrderCancelled records that an order stopped working, and why.
type OrderCancelled struct {
	Envelope
	OrderID      string
	RemainingQty market.Qty
	Reason       CancelReason

	// DecidedAt is when a person asked for this, by their own clock. It is
	// zero on a cancellation the system decided — a remainder the book could
	// not fill, a sibling the other leg cancelled — because nobody decided
	// those, and saying so is the point.
	DecidedAt market.WallClock
}

// ProtectionRefKind says whether a protection is named by the entry that
// planned it or by the episode it now governs.
type ProtectionRefKind uint8

const (
	ProtectionRefEntry ProtectionRefKind = iota + 1
	ProtectionRefEpisode
)

func (k ProtectionRefKind) String() string {
	switch k {
	case ProtectionRefEntry:
		return "entry"
	case ProtectionRefEpisode:
		return "episode"
	default:
		return "unspecified"
	}
}

// ProtectionRef names a protection. It is a tagged union rather than an
// episode identifier with zero meaning "an entry instead": a zero that means
// something is the defect the drawdown accessors had to be rescued from.
type ProtectionRef struct {
	Kind      ProtectionRefKind
	OrderID   string
	EpisodeID uint64
}

func (r ProtectionRef) Validate() error {
	switch r.Kind {
	case ProtectionRefEntry:
		if r.OrderID == "" {
			return ErrProtectionRef
		}
		if r.EpisodeID != 0 {
			return ErrProtectionRef
		}
	case ProtectionRefEpisode:
		if r.EpisodeID == 0 || r.OrderID != "" {
			return ErrProtectionRef
		}
	default:
		return ErrProtectionRef
	}
	return nil
}

// ProtectionEndReason says what the system did to end a protection. These are
// facts about the machine, never about why anyone did anything: naming a
// pattern is analytics and belongs elsewhere. See ADR-008.
type ProtectionEndReason uint8

const (
	ProtectionWithdrawnByTrader ProtectionEndReason = iota + 1
	ProtectionEntryCancelled
	ProtectionDidNotOpenExposure
	ProtectionAlreadyActive
	ProtectionPositionClosed
	ProtectionFlipped
	ProtectionExecuted
)

func (r ProtectionEndReason) String() string {
	switch r {
	case ProtectionWithdrawnByTrader:
		return "withdrawn_by_trader"
	case ProtectionEntryCancelled:
		return "entry_cancelled"
	case ProtectionDidNotOpenExposure:
		return "did_not_open_exposure"
	case ProtectionAlreadyActive:
		return "already_active"
	case ProtectionPositionClosed:
		return "position_closed"
	case ProtectionFlipped:
		return "flipped"
	case ProtectionExecuted:
		return "executed"
	default:
		return "unspecified"
	}
}

// ProtectionPlaced records the levels a trader attached to an entry at the
// moment they submitted it.
//
// It names the entry's order and not an episode, because at the moment of the
// decision there is no episode: the position does not exist until the first
// fill. Requiring the levels afterwards would lose the planned risk at the
// instant it was decided, which is the number the experiment is measured in.
//
// Each level that exists gets an order identifier from a namespace the system
// owns, keyed by this event's own sequence, so a later fill points at something
// that exists. A level that is not set reserves nothing.
type ProtectionPlaced struct {
	Envelope
	EntryOrderID string

	// StopPrice and TargetPrice are zero when not set, as order prices are.
	// Both zero is invalid: it would protect nothing and be indistinguishable
	// from an entry that placed none.
	StopPrice   market.Ticks
	TargetPrice market.Ticks

	// StopOrderID and TargetOrderID are empty for a level that is not set.
	StopOrderID   string
	TargetOrderID string
}

// ProtectionReplaced records levels changed.
//
// Replacement is one event and not a withdrawal followed by a placement,
// because two events would show an interval with no protection that the trader
// never intended, and measuring those intervals is one of the things the log
// exists for.
type ProtectionReplaced struct {
	Envelope
	Ref ProtectionRef

	PreviousStopPrice   market.Ticks
	PreviousTargetPrice market.Ticks
	StopPrice           market.Ticks
	TargetPrice         market.Ticks

	// StopOrderID and TargetOrderID carry the identifiers after the change: the
	// ones already in use where a level survived, and new ones where a level
	// appeared.
	StopOrderID   string
	TargetOrderID string

	// Widened says the stop moved away from the entry. It is a comparison
	// against the level the stop already had, not a judgement about why, and
	// replacing a level from nothing is placing it rather than widening it.
	// Being derived, it is recomputed by Verify rather than believed.
	Widened bool

	// DecidedAt is when the person moved it, by their own clock.
	DecidedAt market.WallClock
}

// ProtectionEnded records a protection that stopped existing, and why.
type ProtectionEnded struct {
	Envelope
	Ref ProtectionRef

	StopPrice   market.Ticks
	TargetPrice market.Ticks
	Reason      ProtectionEndReason

	// DecidedAt is when the person withdrew it, by their own clock, and zero
	// for every ending the system derived from what a fill did.
	DecidedAt market.WallClock
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

// ChallengeDecision records what the challenge engine decided.
//
// The payload is a challenge.Decision and not a challenge.Event, because an
// Event carries its own sequence — the position of the input that caused the
// decision — and nesting it would put two different fields called Sequence in
// one record. The causing position is named for what it is instead.
type ChallengeDecision struct {
	Envelope
	CausedBySequence uint64
	Decision         challenge.Decision
}

// SessionEnded closes a trading session. Reaching the end of a stream is not a
// boundary; ending one is an assertion, as opening one is.
type SessionEnded struct {
	Envelope
	SessionID challenge.SessionID
}

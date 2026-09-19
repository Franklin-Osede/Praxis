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
	KindObservationPresented
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
	case KindObservationPresented:
		return "observation presented"
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

	// Pacing is the condition this journal was produced under. It is
	// configuration and not a screen setting, because a participant who could
	// change it could change the experiment: identical prices are not the same
	// as identical pacing, and two subjects who moved through one file at
	// different speeds are not in the same study. Being here puts it inside
	// the digest a pre-registration records.
	Pacing PacingMode

	// RunID names this execution, and it is supplied rather than minted.
	//
	// Two runs of one configuration over one file are byte-identical — that is a
	// property four determinism tests rest on — so nothing inside can tell them
	// apart, and an anchor keyed on the configuration could be presented against
	// either. A value from crypto/rand would end the byte-identity it exists to
	// repair, so the operator supplies it with --run-id and the journal records
	// it as given.
	//
	// What it cannot do is stop an operator typing one label twice. Then two
	// journals share an identity and the claim empties out. That is protocol
	// discipline, like custody, and it is written in
	// docs/experiment/pilot-protocol.md rather than implied here.
	RunID string
}

// PacingMode says how observations reached the person.
type PacingMode uint8

const (
	// PacingScripted is nobody: a replay, a test, a driven feed.
	PacingScripted PacingMode = iota

	// PacingPilot allows the participant the controls — advancing by hand,
	// pausing, resuming — because wanting to pause is itself data about time
	// pressure and is one of the things the pilots exist to find out. Every
	// use of them is recorded.
	PacingPilot

	// PacingConfirmatory is a fixed automatic cadence: no manual advance, no
	// pause, no rewind, no speed control, and the same information visible to
	// everyone. "Advance by hand with no pause button" is not this — a
	// participant who decides when to press Next can stop for forty seconds
	// first, and removing the button removes the name rather than the
	// behaviour.
	PacingConfirmatory
)

func (p PacingMode) String() string {
	switch p {
	case PacingPilot:
		return "pilot"
	case PacingConfirmatory:
		return "confirmatory"
	default:
		return "scripted"
	}
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

	// Decided is when the person sent it. See Decision: the envelope's time is
	// the market's, and two orders between one tick and the next carry the
	// same one.
	Decided Decision
}

// ObservationPresented records that an observation was put in front of the
// person and their interface said so.
//
// The name is the whole of the claim. It means the browser finished rendering
// and acknowledged it; it does not mean the participant looked at the screen,
// and nothing here should ever be read as saying they did.
//
// It exists because latency needs both ends from one clock. The server stamps
// this when the acknowledgement arrives and stamps a decision when the command
// arrives, so the interval is server-receipt to server-receipt: it includes
// render and transport, which are small and consistent, and it claims nothing
// about the instant a pixel appeared. If the pilots show that separating
// rendering matters, both stamps move into the browser together — never one of
// each, which would make the subtraction meaningless.
type ObservationPresented struct {
	Envelope

	// ObservedSequence is the MarketObserved this put on the screen. With the
	// segment it is the presentation's identity: the same quote presented again
	// in a new segment is a new presentation, because a reload is a new run of
	// interaction and the elapsed reading starts over.
	ObservedSequence uint64

	// Presented is when the acknowledgement reached the server, and it shares
	// the segment of the decisions it will be subtracted from. An interval is
	// only ever computed inside one segment.
	Presented Instant
}

// Instant is a moment on one clock, in the two forms that answer different
// questions. It is what a decision and a presentation have in common, and the
// reason they can be subtracted at all.
type Instant struct {
	// AtUTCNanos is the wall clock, for audit, and is never compared for
	// order: a time server correcting it does not make a journal corrupt.
	AtUTCNanos UnixNanos

	// Segment is the run of uninterrupted interaction this belongs to.
	Segment uint64

	// ElapsedNanos is monotonic since that segment began, and is what an
	// interval is computed from.
	ElapsedNanos ElapsedNanos
}

func (i Instant) IsZero() bool { return i.Segment == 0 }

func (i Instant) Malformed() bool {
	if i.Segment == 0 {
		return i.AtUTCNanos != 0 || i.ElapsedNanos != 0
	}
	return i.AtUTCNanos == 0 || i.ElapsedNanos < 0
}

// UnixNanos is nanoseconds since the Unix epoch, UTC. The unit is in the name
// because a journal is read by people who did not write it, and an integer
// timestamp whose unit has to be inferred is a format that means two things.
type UnixNanos int64

// ElapsedNanos is nanoseconds since a segment of interaction began.
type ElapsedNanos int64

// Decision is a person acting: which gesture it was, and when, in the two forms
// that answer different questions. Neither clock substitutes for the other and
// neither is one the kernel reads: all of it arrives as data from whatever
// witnessed the act.
//
// The market's time is in the envelope and is not this. Two orders sent between
// one tick and the next carry the same envelope time, because the market did
// not move, so the interval a hypothesis about hesitation measures is not there.
//
// A decision is wholly absent or wholly present:
//
//	absent:   no gesture, no moment, no segment, no elapsed
//	present:  a gesture, Segment > 0, ElapsedNanos >= 0, AtUTCNanos != 0
type Decision struct {
	// GestureID names the act, not the thing it acted on, and it is minted by
	// the client at the instant of the gesture. It is what makes every human
	// command idempotent rather than only a submission: a lost response to a
	// replace, retried, arrives under the same gesture and is recognised as
	// the same decision instead of becoming a second one.
	//
	// It is never reused, anywhere in a journal, for the same reason an order
	// identifier is not: a name that has been spent stays attributable.
	GestureID string

	// AtUTCNanos is the participant's own clock. It is for audit — saying when
	// in the world something happened — and it is **never compared for order**.
	// A wall clock can legitimately move backwards: a time server corrects it,
	// an operator sets it, a suspended machine resumes. Treating a corrected
	// clock as a corrupt journal would refuse a session that was entirely
	// honest, and one connection to one kernel does not make a wall clock
	// monotonic.
	AtUTCNanos UnixNanos

	// Segment is a run of uninterrupted interaction, numbered from one, and it
	// only ever goes up. A recovery or a reload starts a new one, because the
	// monotonic reading that made ElapsedNanos meaningful did not survive the
	// interruption.
	//
	// It keeps the chronology coherent; it does not prove that one person held
	// the controls. Two tabs sharing a lease could interleave their gestures
	// inside a single segment without breaking this rule at all — exclusive
	// control is the adapter's invariant and is tested there. Zero means no
	// person was there, which is the honest shape of a scripted run.
	Segment uint64

	// ElapsedNanos is monotonic since its segment began, and it is what an
	// interval is computed from. Within one segment it never goes backwards.
	// Across two it is not subtracted at all: an interval that spanned a
	// recovery is recorded as spanning one, and whether such cases are
	// excluded is a question for the pilots rather than for the engine.
	ElapsedNanos ElapsedNanos
}

// IsZero reports that nobody was there. A segment is numbered from one, so a
// zero segment is an absence rather than a value — an elapsed of zero is
// ordinary, being where every segment starts.
func (d Decision) IsZero() bool { return d.Segment == 0 }

// Malformed reports a decision that is neither wholly absent nor wholly
// present. Without this, a stamp carrying a moment in the world but no segment
// would read as "nobody was there" while plainly recording that somebody was,
// and the half that survived would be the half no interval can be computed
// from.
func (d Decision) Malformed() bool { return malformedDecision(d.instant(), d.GestureID) }

// instant is the moment inside a decision, without the act that names it. The
// chronology is over moments, and a presentation has one too.
func (d Decision) instant() Instant {
	return Instant{AtUTCNanos: d.AtUTCNanos, Segment: d.Segment, ElapsedNanos: d.ElapsedNanos}
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

	// Decided is when a person asked for this. It is zero on a cancellation
	// the system decided — a remainder the book could not fill, a sibling the
	// other leg cancelled — because nobody decided those, and saying so is the
	// point.
	Decided Decision
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

	// ProtectionCoverGone is the last leg cancelled while exposure is still
	// open: a stop that triggered and found nothing, or filled part of the
	// position, with no target beside it. The aggregate has nothing left to
	// offer an observation, and staying Active would report cover over a
	// position that has none.
	ProtectionCoverGone
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
	case ProtectionCoverGone:
		return "cover_gone"
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

	// Decided is when the person moved it.
	Decided Decision
}

// ProtectionEnded records a protection that stopped existing, and why.
type ProtectionEnded struct {
	Envelope
	Ref ProtectionRef

	StopPrice   market.Ticks
	TargetPrice market.Ticks
	Reason      ProtectionEndReason

	// Decided is when the person withdrew it, and zero for every ending the
	// system derived from what a fill did.
	Decided Decision
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

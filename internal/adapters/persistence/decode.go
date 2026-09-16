package persistence

import (
	"fmt"
	"strings"

	"praxis/internal/challenge"
	"praxis/internal/market"
	"praxis/internal/portfolio"
	"praxis/internal/session"
)

// decodeEvent reads one canonical line. It refuses anything it did not write:
// a different spelling of the same number, an unknown enumeration, a missing
// or extra field, or an unknown type.
func decodeEvent(line string, version string) (session.Event, error) {
	if strings.Contains(line, "\r") {
		return nil, fmt.Errorf("%w: carriage return", ErrSyntax)
	}
	parts := strings.Split(line, " ")
	if len(parts) < 3 {
		return nil, fmt.Errorf("%w: %d fields", ErrSyntax, len(parts))
	}
	for _, p := range parts {
		if p == "" {
			return nil, fmt.Errorf("%w: empty field, fields are separated by exactly one space", ErrSyntax)
		}
	}

	r := &reader{parts: parts[1:]}
	at, sequence := r.logicalTime(), r.uint()

	var (
		event session.Event
		kind  session.Kind
	)
	switch parts[0] {
	case typeSessionStarted:
		kind = session.KindSessionStarted
		cfg := session.Config{
			Instrument:               r.instrument(),
			StartingBalanceCts:       r.cents(),
			CommissionPerContractCts: r.cents(),
			Rules: challenge.Rules{
				StartingBalanceCts:  r.cents(),
				MaxDailyLossCts:     r.cents(),
				ProfitTargetCts:     r.cents(),
				MaxTotalLossCts:     r.cents(),
				TrailingDrawdownCts: r.cents(),
			},
		}
		if knows(version, EventVersionV4) {
			cfg.SubjectID = r.optionalID()
			cfg.Pacing = r.pacing()
		}
		event = session.SessionStarted{Envelope: envelope(at, sequence, kind), Config: cfg}

	case typeSessionOpened:
		kind = session.KindSessionOpened
		event = session.SessionOpened{
			Envelope:  envelope(at, sequence, kind),
			SessionID: challenge.SessionID(r.id()), BalanceCts: r.cents(), EquityCts: r.cents(),
		}

	case typeMarketObserved:
		kind = session.KindMarketObserved
		observed := session.MarketObserved{Envelope: envelope(at, sequence, kind)}
		observed.Quote.Instrument, observed.Quote.Time = r.instrument(), r.logicalTime()
		observed.SourceSequence = r.uint()
		observed.Quote.Bid, observed.Quote.Ask = r.ticks(), r.ticks()
		observed.Quote.BidSize, observed.Quote.AskSize = r.qty(), r.qty()
		r.valid(observed.Quote, "quote")
		event = observed

	case typeOrderSubmitted:
		kind = session.KindOrderSubmitted
		order := market.Order{ID: r.id(), Instrument: r.instrument(), Side: r.side(), Type: r.orderType(), Qty: r.qty()}
		order.LimitPrice, order.StopPrice = r.ticks(), r.ticks()
		r.valid(order, "order")
		event = session.OrderSubmitted{
			Envelope: envelope(at, sequence, kind), Order: order,
			Context: session.OrderContext{
				BalanceCts: r.cents(), EquityCts: r.cents(),
				OrdersSubmittedThisSession: r.uint32(), ConsecutiveLosses: r.uint32(),
				SessionRealisedCts: r.cents(), PositionQtyBefore: r.qty(),
			},
		}
		if version != EventVersionV1 {
			submitted := event.(session.OrderSubmitted)
			submitted.Context.ConsecutiveLosingTrades = r.uint32()
			event = submitted
		}
		if knows(version, EventVersionV4) {
			submitted := event.(session.OrderSubmitted)
			submitted.Decided = r.decision()
			event = submitted
		}

	case typeOrderRested:
		kind = session.KindOrderRested
		order := market.Order{ID: r.id(), Instrument: r.instrument(), Side: r.side(), Type: r.orderType(), Qty: r.qty()}
		order.LimitPrice, order.StopPrice = r.ticks(), r.ticks()
		r.valid(order, "order")
		event = session.OrderRested{
			Envelope: envelope(at, sequence, kind), Order: order, RestingQty: r.qty(),
		}

	case typeOrderCancelled:
		kind = session.KindOrderCancelled
		cancelled := session.OrderCancelled{
			Envelope: envelope(at, sequence, kind),
			OrderID:  r.id(), RemainingQty: r.qty(), Reason: r.cancelReason(version),
		}
		if knows(version, EventVersionV4) {
			cancelled.Decided = r.decision()
		}
		event = cancelled

	case typeFillProduced:
		kind = session.KindFillProduced
		fill := market.Fill{
			OrderID: r.id(), Instrument: r.instrument(), Time: r.logicalTime(),
			Side: r.side(), Price: r.ticks(), Qty: r.qty(),
		}
		r.valid(fill, "fill")
		event = session.FillProduced{Envelope: envelope(at, sequence, kind), Fill: fill}

	case typePositionChanged:
		// portfolio.PositionEvent has no Validate, so its fields are checked
		// only for their spelling here. Giving it one is a domain change and
		// not this decoder's to make.
		kind = session.KindPositionChanged
		event = session.PositionChanged{
			Envelope: envelope(at, sequence, kind),
			Change: portfolio.PositionEvent{
				Kind: r.positionKind(), Instrument: r.instrument(), Side: r.side(),
				Qty: r.qty(), Price: r.ticks(), RealisedCts: r.cents(), FeeCts: r.cents(),
			},
		}

	case typeAccountValued:
		kind = session.KindAccountValued
		event = session.AccountValued{
			Envelope:  envelope(at, sequence, kind),
			SessionID: challenge.SessionID(r.id()), BalanceCts: r.cents(), EquityCts: r.cents(),
		}

	case typeChallengeDecision:
		kind = session.KindChallengeDecision
		event = session.ChallengeDecision{
			Envelope: envelope(at, sequence, kind), CausedBySequence: r.uint(),
			Decision: challenge.Decision{
				Kind: r.decisionKind(), SessionID: challenge.SessionID(r.id()),
				BalanceCts: r.cents(), EquityCts: r.cents(),
				LossCts: r.cents(), GainCts: r.cents(), Reason: r.failure(),
				HighWaterCts: r.cents(), ThresholdCts: r.cents(),
			},
		}

	case typeProtectionPlaced:
		if err := requireProtection(version); err != nil {
			return nil, err
		}
		kind = session.KindProtectionPlaced
		event = session.ProtectionPlaced{
			Envelope:     envelope(at, sequence, kind),
			EntryOrderID: r.id(), StopPrice: r.ticks(), TargetPrice: r.ticks(),
			StopOrderID: r.optionalID(), TargetOrderID: r.optionalID(),
		}

	case typeProtectionChanged:
		if err := requireProtection(version); err != nil {
			return nil, err
		}
		kind = session.KindProtectionReplaced
		replaced := session.ProtectionReplaced{Envelope: envelope(at, sequence, kind), Ref: r.ref()}
		replaced.PreviousStopPrice, replaced.PreviousTargetPrice = r.ticks(), r.ticks()
		replaced.StopPrice, replaced.TargetPrice = r.ticks(), r.ticks()
		replaced.StopOrderID, replaced.TargetOrderID = r.optionalID(), r.optionalID()
		replaced.Widened = r.boolean()
		if knows(version, EventVersionV4) {
			replaced.Decided = r.decision()
		}
		event = replaced

	case typeProtectionEnded:
		if err := requireProtection(version); err != nil {
			return nil, err
		}
		kind = session.KindProtectionEnded
		ended := session.ProtectionEnded{Envelope: envelope(at, sequence, kind), Ref: r.ref()}
		ended.StopPrice, ended.TargetPrice = r.ticks(), r.ticks()
		ended.Reason = r.protectionEndReason()
		if knows(version, EventVersionV4) {
			ended.Decided = r.decision()
		}
		event = ended

	case typePresented:
		if !knows(version, EventVersionV4) {
			return nil, fmt.Errorf("%w: %s cannot say an observation was presented",
				ErrUnsupportedInVersion, version)
		}
		kind = session.KindObservationPresented
		presented := session.ObservationPresented{
			Envelope: envelope(at, sequence, kind), ObservedSequence: r.uint(),
		}
		presented.Presented = session.Instant{
			AtUTCNanos:   session.UnixNanos(r.int()),
			Segment:      r.uint(),
			ElapsedNanos: session.ElapsedNanos(r.int()),
		}
		event = presented

	case typeSessionEnded:
		kind = session.KindSessionEnded
		event = session.SessionEnded{Envelope: envelope(at, sequence, kind), SessionID: challenge.SessionID(r.id())}

	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownEvent, parts[0])
	}

	if err := r.done(); err != nil {
		return nil, err
	}
	return event, nil
}

func envelope(at market.LogicalTime, sequence uint64, kind session.Kind) session.Envelope {
	return session.Envelope{Time: at, Sequence: sequence, Kind: kind}
}

// reader consumes a line's fields in order, remembering the first fault and
// requiring at the end that every field was used and none was left over.
type reader struct {
	parts []string
	at    int
	err   error
}

func (r *reader) next() string {
	if r.at >= len(r.parts) {
		r.fail(fmt.Errorf("%w: too few fields", ErrSyntax))
		return ""
	}
	s := r.parts[r.at]
	r.at++
	return s
}

// valid refuses a value that parsed and is not one the domain would accept. A
// decoder does not trust the bytes it reads, and that has to mean the value and
// not only its spelling: the live path checks every one of these at the door,
// and a journal is the one place nothing recomputes them afterwards.
func (r *reader) valid(v interface{ Validate() error }, what string) {
	if err := v.Validate(); err != nil {
		r.fail(fmt.Errorf("%w: %s: %w", ErrNotADomainValue, what, err))
	}
}

func (r *reader) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

func (r *reader) done() error {
	if r.err != nil {
		return r.err
	}
	if r.at != len(r.parts) {
		return fmt.Errorf("%w: %d fields left over", ErrSyntax, len(r.parts)-r.at)
	}
	return nil
}

func (r *reader) int() int64 {
	s := r.next()
	v, err := canonicalInt(s)
	if err != nil {
		r.fail(err)
	}
	return v
}

func (r *reader) uint() uint64 {
	s := r.next()
	v, err := canonicalUint(s)
	if err != nil {
		r.fail(err)
	}
	return v
}

func (r *reader) uint32() uint32 {
	v := r.uint()
	if v > 1<<32-1 {
		r.fail(fmt.Errorf("%w: %d does not fit a counter", ErrSyntax, v))
		return 0
	}
	return uint32(v)
}

func (r *reader) logicalTime() market.LogicalTime { return market.LogicalTime(r.int()) }
func (r *reader) cents() market.Cents             { return market.Cents(r.int()) }
func (r *reader) ticks() market.Ticks             { return market.Ticks(r.int()) }
func (r *reader) qty() market.Qty                 { return market.Qty(r.int()) }
func (r *reader) pacing() session.PacingMode {
	s := r.next()
	for k, name := range pacingNames {
		if name == s {
			return k
		}
	}
	r.fail(fmt.Errorf("%w: pacing mode %q", ErrSyntax, s))
	return 0
}

func (r *reader) decision() session.Decision {
	return session.Decision{
		GestureID:  r.optionalID(),
		AtUTCNanos: session.UnixNanos(r.int()),
		Segment:    r.uint(),

		ElapsedNanos: session.ElapsedNanos(r.int()),
	}
}

// optionalID reads an identifier that may legitimately be absent.
func (r *reader) optionalID() string {
	s := r.next()
	if s == "-" {
		return ""
	}
	if err := validIdentifier(s); err != nil {
		r.fail(err)
	}
	return s
}

func (r *reader) ref() session.ProtectionRef {
	name := r.next()
	orderID, episodeID := r.optionalID(), r.uint()
	for k, n := range protectionRefNames {
		if n == name {
			ref := session.ProtectionRef{Kind: k, OrderID: orderID, EpisodeID: episodeID}
			r.valid(ref, "protection reference")
			return ref
		}
	}
	r.fail(fmt.Errorf("%w: protection reference %q", ErrSyntax, name))
	return session.ProtectionRef{}
}

func (r *reader) protectionEndReason() session.ProtectionEndReason {
	s := r.next()
	for k, name := range protectionEndNames {
		if name == s {
			return k
		}
	}
	r.fail(fmt.Errorf("%w: protection end reason %q", ErrSyntax, s))
	return 0
}

func (r *reader) boolean() bool {
	switch s := r.next(); s {
	case "0":
		return false
	case "1":
		return true
	default:
		r.fail(fmt.Errorf("%w: boolean %q, want 0 or 1", ErrSyntax, s))
		return false
	}
}

func (r *reader) id() string {
	s := r.next()
	if err := validIdentifier(s); err != nil {
		r.fail(err)
	}
	return s
}

func (r *reader) instrument() market.Instrument {
	i := market.Instrument{Symbol: r.id(), CentsPerTick: r.cents()}
	r.valid(i, "instrument")
	return i
}

func (r *reader) side() market.Side {
	s := r.next()
	for k, name := range sideNames {
		if name == s {
			return k
		}
	}
	r.fail(fmt.Errorf("%w: side %q", ErrSyntax, s))
	return market.SideUnspecified
}

// cancelReason reads a reason the payload's own version has a name for.
//
// A reason introduced later is refused rather than accepted, because a reader
// that took it would be reading a file it does not understand and saying it
// did: the version on the container is the reader's only warning that the
// bytes may hold something it cannot represent.
func (r *reader) cancelReason(version string) session.CancelReason {
	s := r.next()
	for k, name := range cancelReasonNames {
		if name != s {
			continue
		}
		if since, later := cancelReasonsSince[k]; later && !knows(version, since) {
			r.fail(fmt.Errorf("%w: %s has no name for %q", ErrUnsupportedInVersion, version, s))
			return 0
		}
		return k
	}
	r.fail(fmt.Errorf("%w: cancellation reason %q", ErrSyntax, s))
	return 0
}

func (r *reader) orderType() market.OrderType {
	s := r.next()
	for k, name := range orderTypeNames {
		if name == s {
			return k
		}
	}
	r.fail(fmt.Errorf("%w: order type %q", ErrSyntax, s))
	return market.OrderTypeUnspecified
}

func (r *reader) positionKind() portfolio.PositionEventKind {
	s := r.next()
	for k, name := range positionKindNames {
		if name == s {
			return k
		}
	}
	r.fail(fmt.Errorf("%w: position change %q", ErrSyntax, s))
	return 0
}

func (r *reader) decisionKind() challenge.EventKind {
	s := r.next()
	for k, name := range decisionKindNames {
		if name == s {
			return k
		}
	}
	r.fail(fmt.Errorf("%w: decision %q", ErrSyntax, s))
	return 0
}

func (r *reader) failure() challenge.FailureReason {
	s := r.next()
	for k, name := range failureNames {
		if name == s {
			return k
		}
	}
	r.fail(fmt.Errorf("%w: failure reason %q", ErrSyntax, s))
	return challenge.FailureNone
}

// canonicalInt refuses every alternative spelling of a number: a leading plus,
// a leading zero, a negative zero, or anything strconv would tolerate that this
// format does not. Normalising them silently would let two different files mean
// the same thing.
// canonicalInt and canonicalUint read the domain's spelling of an integer. The
// rule is market's, because a Cents and a Ticks are market's and a journal is
// one of three places that write them down; this asserts it rather than
// deciding it a second time.
func canonicalInt(s string) (int64, error)   { return market.ParseInt(s) }
func canonicalUint(s string) (uint64, error) { return market.ParseUint(s) }

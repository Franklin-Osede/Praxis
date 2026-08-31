package persistence

import (
	"fmt"
	"strconv"
	"strings"

	"praxis/internal/challenge"
	"praxis/internal/market"
	"praxis/internal/portfolio"
	"praxis/internal/session"
)

// decodeEvent reads one canonical line. It refuses anything it did not write:
// a different spelling of the same number, an unknown enumeration, a missing
// or extra field, or an unknown type.
func decodeEvent(line string) (session.Event, error) {
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
			Instrument:               market.Instrument{Symbol: r.id(), CentsPerTick: r.cents()},
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
		event = session.SessionStarted{Envelope: envelope(at, sequence, kind), Config: cfg}

	case typeSessionOpened:
		kind = session.KindSessionOpened
		event = session.SessionOpened{
			Envelope:  envelope(at, sequence, kind),
			SessionID: challenge.SessionID(r.id()), BalanceCts: r.cents(), EquityCts: r.cents(),
		}

	case typeMarketObserved:
		kind = session.KindMarketObserved
		event = session.MarketObserved{
			Envelope: envelope(at, sequence, kind),
			Quote: market.Quote{
				Instrument: r.instrument(), Time: r.logicalTime(),
				Bid: r.ticks(), Ask: r.ticks(), BidSize: r.qty(), AskSize: r.qty(),
			},
		}

	case typeOrderSubmitted:
		kind = session.KindOrderSubmitted
		order := market.Order{ID: r.id(), Instrument: r.instrument(), Side: r.side(), Type: r.orderType(), Qty: r.qty()}
		order.LimitPrice, order.StopPrice = r.ticks(), r.ticks()
		event = session.OrderSubmitted{
			Envelope: envelope(at, sequence, kind), Order: order,
			Context: session.OrderContext{
				BalanceCts: r.cents(), EquityCts: r.cents(),
				OrdersSubmittedThisSession: r.uint32(), ConsecutiveLosses: r.uint32(),
				SessionRealisedCts: r.cents(), PositionQtyBefore: r.qty(),
			},
		}

	case typeFillProduced:
		kind = session.KindFillProduced
		event = session.FillProduced{
			Envelope: envelope(at, sequence, kind),
			Fill: market.Fill{
				OrderID: r.id(), Instrument: r.instrument(), Time: r.logicalTime(),
				Side: r.side(), Price: r.ticks(), Qty: r.qty(),
			},
		}

	case typePositionChanged:
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

func (r *reader) id() string {
	s := r.next()
	if err := validIdentifier(s); err != nil {
		r.fail(err)
	}
	return s
}

func (r *reader) instrument() market.Instrument {
	return market.Instrument{Symbol: r.id(), CentsPerTick: r.cents()}
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
func canonicalInt(s string) (int64, error) {
	digits := s
	negative := strings.HasPrefix(s, "-")
	if negative {
		digits = s[1:]
	}
	if err := canonicalDigits(s, digits, negative); err != nil {
		return 0, err
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q: %v", ErrNotCanonicalInt, s, err)
	}
	return v, nil
}

func canonicalUint(s string) (uint64, error) {
	if err := canonicalDigits(s, s, false); err != nil {
		return 0, err
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q: %v", ErrNotCanonicalInt, s, err)
	}
	return v, nil
}

func canonicalDigits(whole, digits string, negative bool) error {
	if digits == "" {
		return fmt.Errorf("%w: %q has no digits", ErrNotCanonicalInt, whole)
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return fmt.Errorf("%w: %q", ErrNotCanonicalInt, whole)
		}
	}
	if len(digits) > 1 && digits[0] == '0' {
		return fmt.Errorf("%w: %q has a leading zero", ErrNotCanonicalInt, whole)
	}
	if negative && digits == "0" {
		return fmt.Errorf("%w: negative zero", ErrNotCanonicalInt)
	}
	return nil
}

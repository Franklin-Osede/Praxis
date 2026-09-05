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

// Event type names. A name is the event's kind: the kind is never written as a
// separate field, so a payload cannot contain a line whose tag contradicts its
// shape.
const (
	typeSessionStarted    = "session_started"
	typeSessionOpened     = "session_opened"
	typeMarketObserved    = "market_observed"
	typeOrderSubmitted    = "order_submitted"
	typeOrderRested       = "order_rested"
	typeOrderCancelled    = "order_cancelled"
	typeProtectionPlaced  = "protection_placed"
	typeProtectionChanged = "protection_replaced"
	typeProtectionRemoved = "protection_cancelled"
	typeFillProduced      = "fill_produced"
	typePositionChanged   = "position_changed"
	typeAccountValued     = "account_valued"
	typeChallengeDecision = "challenge_decision"
	typeSessionEnded      = "session_ended"
)

// Enumeration names owned by this package.
var (
	sideNames         = map[market.Side]string{market.SideBuy: "buy", market.SideSell: "sell"}
	orderTypeNames    = map[market.OrderType]string{market.OrderTypeMarket: "market", market.OrderTypeLimit: "limit", market.OrderTypeStop: "stop"}
	positionKindNames = map[portfolio.PositionEventKind]string{
		portfolio.PositionOpened: "opened", portfolio.PositionIncreased: "increased",
		portfolio.PositionReduced: "reduced", portfolio.PositionClosed: "closed",
	}
	cancelReasonNames = map[session.CancelReason]string{
		session.CancelledByTrader:            "by_trader",
		session.CancelledUnfillableRemainder: "unfillable_remainder",
	}
	decisionKindNames = map[challenge.EventKind]string{
		challenge.ChallengeActivated: "activated", challenge.SessionReferenceEstablished: "session_reference_established",
		challenge.ChallengeFailed: "failed", challenge.ChallengePassed: "passed",
	}
	failureNames = map[challenge.FailureReason]string{
		challenge.FailureNone: "none", challenge.FailureDailyLoss: "daily_loss",
		challenge.FailureStaticDrawdown: "static_drawdown", challenge.FailureTrailingDrawdown: "trailing_drawdown",
	}
)

func encodeEvent(e session.Event, version string) (string, error) {
	header := e.Header()
	f := &fields{}

	switch v := e.(type) {
	case session.SessionStarted:
		if header.Kind != session.KindSessionStarted {
			return "", ErrKindMismatch
		}
		f.name(typeSessionStarted).at(header)
		f.id(v.Config.Instrument.Symbol).int(int64(v.Config.Instrument.CentsPerTick))
		f.int(int64(v.Config.StartingBalanceCts)).int(int64(v.Config.CommissionPerContractCts))
		// challenge.Rules, in this order and no other.
		f.int(int64(v.Config.Rules.StartingBalanceCts))
		f.int(int64(v.Config.Rules.MaxDailyLossCts))
		f.int(int64(v.Config.Rules.ProfitTargetCts))
		f.int(int64(v.Config.Rules.MaxTotalLossCts))
		f.int(int64(v.Config.Rules.TrailingDrawdownCts))

	case session.SessionOpened:
		if header.Kind != session.KindSessionOpened {
			return "", ErrKindMismatch
		}
		f.name(typeSessionOpened).at(header)
		f.id(string(v.SessionID)).int(int64(v.BalanceCts)).int(int64(v.EquityCts))

	case session.MarketObserved:
		if header.Kind != session.KindMarketObserved {
			return "", ErrKindMismatch
		}
		f.name(typeMarketObserved).at(header)
		f.instrument(v.Quote.Instrument).int(int64(v.Quote.Time)).uint(v.SourceSequence)
		f.int(int64(v.Quote.Bid)).int(int64(v.Quote.Ask))
		f.int(int64(v.Quote.BidSize)).int(int64(v.Quote.AskSize))

	case session.OrderSubmitted:
		if header.Kind != session.KindOrderSubmitted {
			return "", ErrKindMismatch
		}
		f.name(typeOrderSubmitted).at(header)
		f.id(v.Order.ID).instrument(v.Order.Instrument)
		f.side(v.Order.Side).orderType(v.Order.Type).int(int64(v.Order.Qty))
		f.int(int64(v.Order.LimitPrice)).int(int64(v.Order.StopPrice))
		f.int(int64(v.Context.BalanceCts)).int(int64(v.Context.EquityCts))
		f.uint(uint64(v.Context.OrdersSubmittedThisSession)).uint(uint64(v.Context.ConsecutiveLosses))
		f.int(int64(v.Context.SessionRealisedCts)).int(int64(v.Context.PositionQtyBefore))
		if version != EventVersionV1 {
			f.uint(uint64(v.Context.ConsecutiveLosingTrades))
		} else if v.Context.ConsecutiveLosingTrades != 0 {
			return "", fmt.Errorf("%w: %s cannot carry a losing-trade streak", ErrUnsupportedInVersion, version)
		}

	case session.OrderRested:
		if header.Kind != session.KindOrderRested {
			return "", ErrKindMismatch
		}
		f.name(typeOrderRested).at(header)
		f.id(v.Order.ID).instrument(v.Order.Instrument)
		f.side(v.Order.Side).orderType(v.Order.Type).int(int64(v.Order.Qty))
		f.int(int64(v.Order.LimitPrice)).int(int64(v.Order.StopPrice))
		f.int(int64(v.RestingQty))

	case session.OrderCancelled:
		if header.Kind != session.KindOrderCancelled {
			return "", ErrKindMismatch
		}
		f.name(typeOrderCancelled).at(header)
		f.id(v.OrderID).int(int64(v.RemainingQty)).cancelReason(v.Reason)

	case session.FillProduced:
		if header.Kind != session.KindFillProduced {
			return "", ErrKindMismatch
		}
		f.name(typeFillProduced).at(header)
		f.id(v.Fill.OrderID).instrument(v.Fill.Instrument).int(int64(v.Fill.Time))
		f.side(v.Fill.Side).int(int64(v.Fill.Price)).int(int64(v.Fill.Qty))

	case session.PositionChanged:
		if header.Kind != session.KindPositionChanged {
			return "", ErrKindMismatch
		}
		f.name(typePositionChanged).at(header)
		f.positionKind(v.Change.Kind).instrument(v.Change.Instrument)
		f.side(v.Change.Side).int(int64(v.Change.Qty)).int(int64(v.Change.Price))
		f.int(int64(v.Change.RealisedCts)).int(int64(v.Change.FeeCts))

	case session.AccountValued:
		if header.Kind != session.KindAccountValued {
			return "", ErrKindMismatch
		}
		f.name(typeAccountValued).at(header)
		f.id(string(v.SessionID)).int(int64(v.BalanceCts)).int(int64(v.EquityCts))

	case session.ChallengeDecision:
		if header.Kind != session.KindChallengeDecision {
			return "", ErrKindMismatch
		}
		f.name(typeChallengeDecision).at(header)
		f.uint(v.CausedBySequence).decisionKind(v.Decision.Kind).id(string(v.Decision.SessionID))
		f.int(int64(v.Decision.BalanceCts)).int(int64(v.Decision.EquityCts))
		f.int(int64(v.Decision.LossCts)).int(int64(v.Decision.GainCts))
		f.failure(v.Decision.Reason)
		f.int(int64(v.Decision.HighWaterCts)).int(int64(v.Decision.ThresholdCts))

	case session.ProtectionPlaced:
		if version == EventVersionV1 {
			return "", fmt.Errorf("%w: %s has no protection", ErrUnsupportedInVersion, version)
		}
		if header.Kind != session.KindProtectionPlaced {
			return "", ErrKindMismatch
		}
		f.name(typeProtectionPlaced).at(header)
		f.id(v.EntryOrderID).int(int64(v.StopPrice)).int(int64(v.TargetPrice))

	case session.ProtectionReplaced:
		if version == EventVersionV1 {
			return "", fmt.Errorf("%w: %s has no protection", ErrUnsupportedInVersion, version)
		}
		if header.Kind != session.KindProtectionReplaced {
			return "", ErrKindMismatch
		}
		f.name(typeProtectionChanged).at(header)
		f.uint(v.EpisodeID)
		f.int(int64(v.PreviousStopPrice)).int(int64(v.PreviousTargetPrice))
		f.int(int64(v.StopPrice)).int(int64(v.TargetPrice)).boolean(v.Widened)

	case session.ProtectionCancelled:
		if version == EventVersionV1 {
			return "", fmt.Errorf("%w: %s has no protection", ErrUnsupportedInVersion, version)
		}
		if header.Kind != session.KindProtectionCancelled {
			return "", ErrKindMismatch
		}
		f.name(typeProtectionRemoved).at(header)
		f.uint(v.EpisodeID).int(int64(v.StopPrice)).int(int64(v.TargetPrice))

	case session.SessionEnded:
		if header.Kind != session.KindSessionEnded {
			return "", ErrKindMismatch
		}
		f.name(typeSessionEnded).at(header).id(string(v.SessionID))

	default:
		return "", fmt.Errorf("%w: %T", ErrUnknownEvent, e)
	}

	return f.done()
}

// fields builds one line, collecting the first error rather than returning one
// at every step.
type fields struct {
	parts []string
	err   error
}

func (f *fields) name(s string) *fields { f.parts = append(f.parts, s); return f }

func (f *fields) at(h session.Envelope) *fields {
	return f.int(int64(h.Time)).uint(h.Sequence)
}

func (f *fields) int(v int64) *fields {
	f.parts = append(f.parts, strconv.FormatInt(v, 10))
	return f
}

func (f *fields) uint(v uint64) *fields {
	f.parts = append(f.parts, strconv.FormatUint(v, 10))
	return f
}

// boolean has one spelling and only one, so a payload cannot say the same
// thing two ways.
func (f *fields) boolean(v bool) *fields {
	if v {
		return f.name("1")
	}
	return f.name("0")
}

func (f *fields) id(s string) *fields {
	if err := validIdentifier(s); err != nil && f.err == nil {
		f.err = err
	}
	f.parts = append(f.parts, s)
	return f
}

func (f *fields) instrument(i market.Instrument) *fields {
	return f.id(i.Symbol).int(int64(i.CentsPerTick))
}

func (f *fields) side(s market.Side) *fields { return f.enum(sideNames[s], "side") }
func (f *fields) orderType(t market.OrderType) *fields {
	return f.enum(orderTypeNames[t], "order type")
}
func (f *fields) positionKind(k portfolio.PositionEventKind) *fields {
	return f.enum(positionKindNames[k], "position change")
}
func (f *fields) decisionKind(k challenge.EventKind) *fields {
	return f.enum(decisionKindNames[k], "decision")
}
func (f *fields) failure(r challenge.FailureReason) *fields {
	return f.enum(failureNames[r], "failure reason")
}

func (f *fields) cancelReason(r session.CancelReason) *fields {
	return f.enum(cancelReasonNames[r], "cancellation reason")
}

func (f *fields) enum(name, what string) *fields {
	if name == "" && f.err == nil {
		f.err = fmt.Errorf("%w: no name for this %s", ErrSyntax, what)
	}
	f.parts = append(f.parts, name)
	return f
}

func (f *fields) done() (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return strings.Join(f.parts, " "), nil
}

// identifierAllowed is the whole escaping rule: a character outside it cannot
// appear in an identifier, so no identifier ever needs quoting.
func identifierAllowed(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '.', r == '_', r == ':', r == '-':
		return true
	}
	return false
}

func validIdentifier(s string) error {
	if s == "" {
		return fmt.Errorf("%w: identifier is empty", ErrIdentifier)
	}
	for _, r := range s {
		if !identifierAllowed(r) {
			return fmt.Errorf("%w: %q in %q", ErrIdentifier, r, s)
		}
	}
	return nil
}

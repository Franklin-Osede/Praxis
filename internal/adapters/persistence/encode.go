package persistence

import (
	"fmt"
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
	typeProtectionEnded   = "protection_ended"
	typeFillProduced      = "fill_produced"
	typePositionChanged   = "position_changed"
	typeAccountValued     = "account_valued"
	typeChallengeDecision = "challenge_decision"
	typeSessionEnded      = "session_ended"
	typePresented         = "observation_presented"
)

// Enumeration names owned by this package.
var (
	sideNames         = map[market.Side]string{market.SideBuy: "buy", market.SideSell: "sell"}
	orderTypeNames    = map[market.OrderType]string{market.OrderTypeMarket: "market", market.OrderTypeLimit: "limit", market.OrderTypeStop: "stop"}
	positionKindNames = map[portfolio.PositionEventKind]string{
		portfolio.PositionOpened: "opened", portfolio.PositionIncreased: "increased",
		portfolio.PositionReduced: "reduced", portfolio.PositionClosed: "closed",
	}
	protectionRefNames = map[session.ProtectionRefKind]string{
		session.ProtectionRefEntry:   "entry",
		session.ProtectionRefEpisode: "episode",
	}
	protectionEndNames = map[session.ProtectionEndReason]string{
		session.ProtectionWithdrawnByTrader:  "withdrawn_by_trader",
		session.ProtectionEntryCancelled:     "entry_cancelled",
		session.ProtectionDidNotOpenExposure: "did_not_open_exposure",
		session.ProtectionAlreadyActive:      "already_active",
		session.ProtectionPositionClosed:     "position_closed",
		session.ProtectionFlipped:            "flipped",
		session.ProtectionExecuted:           "executed",
		session.ProtectionCoverGone:          "cover_gone",
	}

	// protectionEndsSince is cancelReasonsSince for endings: a reader of an
	// older version has no name for the ending a protection left with no leg
	// over open exposure gets, so an older writer refuses it.
	protectionEndsSince = map[session.ProtectionEndReason]string{
		session.ProtectionCoverGone: EventVersionV6,
	}
	pacingNames = map[session.PacingMode]string{
		session.PacingScripted:     "scripted",
		session.PacingPilot:        "pilot",
		session.PacingConfirmatory: "confirmatory",
	}
	cancelReasonNames = map[session.CancelReason]string{
		session.CancelledByTrader:            "by_trader",
		session.CancelledUnfillableRemainder: "unfillable_remainder",
		session.CancelledByOCO:               "by_oco",
		session.CancelledPositionClosed:      "position_closed",
	}

	// cancelReasonsSince says which version first had a name for a reason. A
	// reader of an older version has no name for it, so an older writer must
	// refuse it rather than produce a line that reader would reject.
	cancelReasonsSince = map[session.CancelReason]string{
		session.CancelledByOCO:          EventVersionV4,
		session.CancelledPositionClosed: EventVersionV4,
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
		if knows(version, EventVersionV4) {
			f.optionalID(v.Config.SubjectID)
			f.enum(pacingNames[v.Config.Pacing], "pacing mode")
		} else if v.Config.SubjectID != "" || v.Config.Pacing != session.PacingScripted {
			return "", fmt.Errorf("%w: %s cannot say who traded it, or how",
				ErrUnsupportedInVersion, version)
		}
		// A run identity is absent for a scripted journal and required for one
		// somebody traded, so "-" is the spelling for absent, as it is for the
		// subject. An older version carrying one would be a journal claiming an
		// identity its readers cannot see.
		if knows(version, EventVersionV5) {
			f.optionalID(v.Config.RunID)
		} else if v.Config.RunID != "" {
			return "", fmt.Errorf("%w: %s cannot say which run it is",
				ErrUnsupportedInVersion, version)
		}

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
		if err := f.decidedAt(version, v.Decided); err != nil {
			return "", err
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
		if err := requireCancelReason(version, v.Reason); err != nil {
			return "", err
		}
		f.name(typeOrderCancelled).at(header)
		f.id(v.OrderID).int(int64(v.RemainingQty)).cancelReason(v.Reason)
		if err := f.decidedAt(version, v.Decided); err != nil {
			return "", err
		}

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
		if err := requireProtection(version); err != nil {
			return "", err
		}
		if header.Kind != session.KindProtectionPlaced {
			return "", ErrKindMismatch
		}
		f.name(typeProtectionPlaced).at(header)
		f.id(v.EntryOrderID).int(int64(v.StopPrice)).int(int64(v.TargetPrice))
		f.optionalID(v.StopOrderID).optionalID(v.TargetOrderID)

	case session.ProtectionReplaced:
		if err := requireProtection(version); err != nil {
			return "", err
		}
		if header.Kind != session.KindProtectionReplaced {
			return "", ErrKindMismatch
		}
		f.name(typeProtectionChanged).at(header).ref(v.Ref)
		f.int(int64(v.PreviousStopPrice)).int(int64(v.PreviousTargetPrice))
		f.int(int64(v.StopPrice)).int(int64(v.TargetPrice))
		f.optionalID(v.StopOrderID).optionalID(v.TargetOrderID).boolean(v.Widened)
		if err := f.decidedAt(version, v.Decided); err != nil {
			return "", err
		}

	case session.ProtectionEnded:
		if err := requireProtection(version); err != nil {
			return "", err
		}
		if header.Kind != session.KindProtectionEnded {
			return "", ErrKindMismatch
		}
		if err := requireProtectionEnd(version, v.Reason); err != nil {
			return "", err
		}
		f.name(typeProtectionEnded).at(header).ref(v.Ref)
		f.int(int64(v.StopPrice)).int(int64(v.TargetPrice))
		f.enum(protectionEndNames[v.Reason], "protection end reason")
		if err := f.decidedAt(version, v.Decided); err != nil {
			return "", err
		}

	case session.ObservationPresented:
		if header.Kind != session.KindObservationPresented {
			return "", ErrKindMismatch
		}
		if !knows(version, EventVersionV4) {
			return "", fmt.Errorf("%w: %s cannot say an observation was presented",
				ErrUnsupportedInVersion, version)
		}
		f.name(typePresented).at(header)
		f.uint(v.ObservedSequence)
		f.int(int64(v.Presented.AtUTCNanos)).uint(v.Presented.Segment).int(int64(v.Presented.ElapsedNanos))

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
	f.parts = append(f.parts, market.FormatInt(v))
	return f
}

func (f *fields) uint(v uint64) *fields {
	f.parts = append(f.parts, market.FormatUint(v))
	return f
}

// decidedAt writes when a person acted, which only the version that has a name
// for it can carry. An older version refuses a non-zero one rather than
// dropping it: a journal that silently lost when its decisions were taken would
// be a behavioural record missing the behaviour.
//
// Four fields: which gesture it was, the moment in the world, the run of
// interaction it belongs to, and the monotonic reading within that run. The two
// clocks answer different questions and neither substitutes for the other. See
// session.Decision.
func (f *fields) decidedAt(version string, d session.Decision) error {
	if knows(version, EventVersionV4) {
		f.optionalID(d.GestureID).int(int64(d.AtUTCNanos)).uint(d.Segment).int(int64(d.ElapsedNanos))
		return nil
	}
	if !d.IsZero() {
		return fmt.Errorf("%w: %s cannot say when a person acted", ErrUnsupportedInVersion, version)
	}
	return nil
}

func requireProtection(version string) error {
	if version == EventVersionV1 || version == EventVersionV2 {
		return fmt.Errorf("%w: %s has no protection", ErrUnsupportedInVersion, version)
	}
	return nil
}

// versionOrder is how versions compare. It is an explicit table and not a
// parse of the string, because an ordering derived from a name would start
// deciding things about names nobody chose it to decide.
var versionOrder = map[string]int{
	EventVersionV1: 1, EventVersionV2: 2, EventVersionV3: 3, EventVersionV4: 4,
	EventVersionV5: 5, EventVersionV6: 6,
}

func knows(version string, since string) bool {
	return versionOrder[version] >= versionOrder[since]
}

func requireProtectionEnd(version string, reason session.ProtectionEndReason) error {
	since, later := protectionEndsSince[reason]
	if later && !knows(version, since) {
		return fmt.Errorf("%w: %s has no name for %q", ErrUnsupportedInVersion, version, protectionEndNames[reason])
	}
	return nil
}

func requireCancelReason(version string, reason session.CancelReason) error {
	since, later := cancelReasonsSince[reason]
	if later && !knows(version, since) {
		return fmt.Errorf("%w: %s has no name for %q", ErrUnsupportedInVersion, version, cancelReasonNames[reason])
	}
	return nil
}

// optionalID writes an identifier that may legitimately be absent. A level
// that is not set reserves no name, and "-" says so in one spelling.
func (f *fields) optionalID(s string) *fields {
	if s == "" {
		return f.name("-")
	}
	return f.id(s)
}

func (f *fields) ref(r session.ProtectionRef) *fields {
	f.enum(protectionRefNames[r.Kind], "protection reference")
	if r.Kind == session.ProtectionRefEntry {
		return f.optionalID(r.OrderID).uint(0)
	}
	return f.optionalID("").uint(r.EpisodeID)
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
// validIdentifier is the format's own gate, and it is deliberately a second
// implementation of market.ValidIdentifier rather than a call to it.
//
// The domain refuses these names so that a command the record cannot hold is
// never accepted in the first place. This one refuses them again because a
// decoder must not trust the bytes it is reading, and because the format's
// obligation stands whatever the domain later decides. TestTheDomainAndTheFormat
// AgreeOnIdentifiers holds the two to the same set.
func validIdentifier(s string) error {
	if s == "" {
		return fmt.Errorf("%w: identifier is empty", ErrIdentifier)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == ':', r == '-':
		default:
			return fmt.Errorf("%w: %q in %q", ErrIdentifier, r, s)
		}
	}
	return nil
}

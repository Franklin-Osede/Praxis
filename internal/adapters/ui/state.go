package ui

import (
	"strconv"
	"strings"

	"praxis/internal/challenge"
	"praxis/internal/market"
	"praxis/internal/session"
)

// State is what the participant may know, and nothing else.
//
// It is a projection designed for the protocol, not a serialisation of the
// session. The distinction is the experiment's, not the engineer's: a field
// present in the payload is available in the browser's developer tools whatever
// the stylesheet does, so **for the experiment, sent is shown**. Adding the
// distance to a drawdown threshold here would change what a hypothesis about
// rule breaches on losing days is measuring — from "people break rules when
// already down" to "people react to a number they were shown".
//
// The inventory is section 11 of docs/PRAXIS_SPEC.md and is deliberately short:
// the book, the position, the money, the evaluation's state, the losing streak,
// what is working, what is protected, and where the feed is. No rules, no
// hidden thresholds, no analytical fields.
type State struct {
	Subject string `json:"subject"`
	Pacing  string `json:"pacing"`

	// Cursor is how many observations have been consumed, and Observations how
	// many the file holds. Together they are where the session is.
	Cursor       int    `json:"cursor"`
	Observations int    `json:"observations"`
	SessionOpen  bool   `json:"sessionOpen"`
	SessionID    string `json:"sessionId"`

	// ObservedSequence names the observation on the screen, so a confirmation
	// can say which one it confirms. Zero — absent — when none is.
	ObservedSequence string `json:"observedSequence,omitempty"`

	Book     *Book    `json:"book"`
	Position Position `json:"position"`
	// Money is absent rather than empty when there is none to show, the same
	// way Book is. An empty string is not a decimal, and a client parsing one
	// gets NaN beside the notice telling it something is wrong.
	Money      *Money     `json:"money,omitempty"`
	Evaluation Evaluation `json:"evaluation"`

	// ConsecutiveLosingTrades is here because the journal claims the
	// participant knew it. OrderContext records it as what the trader knew,
	// and the only thing that can make that true is the screen.
	ConsecutiveLosingTrades uint32 `json:"consecutiveLosingTrades"`

	// Fills is what filled while the row on the screen was the row on the
	// screen, in the order it happened. A manual close and a stop-out leave the
	// same numbers — position flat, money moved — and a participant who cannot
	// tell them apart takes their next decision on a misreading the journal
	// cannot separate afterwards. It is on the inventory in §11 for that
	// reason, and it says nothing the journal does not already record.
	//
	// The row and not the last fill: two orders can fill on one observation,
	// and naming one of them describes a smaller event than the one that
	// happened. It empties when the market moves on, because it is about what
	// is on the screen.
	Fills []Fill `json:"fills,omitempty"`

	Working    []Order      `json:"working"`
	Protection []Protection `json:"protection"`

	// NeedsRecovery is why the interface stopped, and it carries two different
	// facts. A session that has stopped will accept nothing further: that one
	// is terminal for the session. A valuation that could not be taken is not
	// — the session is alive — but it is terminal for the screen, which may
	// show nothing and may not show something else in money's place.
	//
	// When both hold, the stopped session is what is reported. It is the more
	// fundamental of the two and the one an operator acts on; a valuation
	// failing inside a dead session is a consequence of it, not a second thing
	// to fix.
	NeedsRecovery string `json:"needsRecovery,omitempty"`
}

// Every quantity crosses as a decimal string. JavaScript numbers are binary
// floating point, and a monetary boundary that went through one would be a
// second money representation with its own rounding — the thing this project
// spends most of its integer discipline avoiding. Formatting lives here, in Go,
// for the same reason: a toFixed in the browser is a second implementation of
// the rule and will eventually disagree with the first.
type Book struct {
	Time    string `json:"time"`
	Bid     string `json:"bid"`
	Ask     string `json:"ask"`
	BidSize string `json:"bidSize"`
	AskSize string `json:"askSize"`
}

type Position struct {
	Symbol string `json:"symbol"`
	NetQty string `json:"netQty"`
}

type Money struct {
	BalanceCts string `json:"balanceCts"`
	EquityCts  string `json:"equityCts"`
}

type Evaluation struct {
	State  string `json:"state"`
	Reason string `json:"reason"`
}

type Order struct {
	ID         string `json:"id"`
	Side       string `json:"side"`
	Type       string `json:"type"`
	Qty        string `json:"qty"`
	LimitPrice string `json:"limitPrice"`
	StopPrice  string `json:"stopPrice"`
}

// Fill names one fill and everything it did to the position.
type Fill struct {
	// Cause is "order" for something the participant submitted, "stop" or
	// "target" for a protective leg doing its work.
	Cause string `json:"cause"`
	Side  string `json:"side"`
	Qty   string `json:"qty"`
	Price string `json:"price"`

	// Changes are the legs the fill produced, in order. A reversal is two: the
	// position closed and the opposite one opened. Reporting only the first
	// told a participant their position had closed while they held the other
	// side of it, with the position row beside it saying otherwise.
	Changes []PositionChange `json:"changes"`
}

// PositionChange is one leg of what a fill did.
type PositionChange struct {
	// Kind is opened, increased, reduced or closed.
	Kind string `json:"kind"`
	Qty  string `json:"qty"`
}

type Protection struct {
	// Status is "planned" or "active": a plan waits for its entry, and an
	// active protection is bound to the position it covers.
	Status       string `json:"status"`
	EntryOrderID string `json:"entryOrderId,omitempty"`
	EpisodeID    string `json:"episodeId,omitempty"`
	StopPrice    string `json:"stopPrice"`
	TargetPrice  string `json:"targetPrice"`
	ProtectedQty string `json:"protectedQty,omitempty"`
}

// fillsOnScreen reads the fills the row on the screen produced, with what each
// did to the position. Nothing is derived: the fill, the changes it caused and
// the name of the order that filled are all recorded, and a protective leg
// carries the reserved name the session gave it.
//
// It walks back to the observation on the screen and forward from there, so a
// fill belongs to the row it happened on and goes when that row does.
func fillsOnScreen(events []session.Event) []Fill {
	from := 0
	for n := len(events) - 1; n >= 0; n-- {
		if _, observed := events[n].(session.MarketObserved); observed {
			from = n + 1
			break
		}
	}

	var fills []Fill
	for _, e := range events[from:] {
		switch v := e.(type) {
		case session.FillProduced:
			fills = append(fills, Fill{
				Cause: causeOf(v.Fill.OrderID),
				Side:  v.Fill.Side.String(),
				Qty:   decimal(int64(v.Fill.Qty)),
				Price: decimal(int64(v.Fill.Price)),
			})
		case session.PositionChanged:
			// Every change belongs to the fill before it, and a reversal
			// produces two of them from one.
			if len(fills) == 0 {
				continue
			}
			last := &fills[len(fills)-1]
			last.Changes = append(last.Changes, PositionChange{
				Kind: v.Change.Kind.String(),
				Qty:  decimal(int64(v.Change.Qty)),
			})
		}
	}
	return fills
}

// causeOf says whose order filled. The session names a protective leg from a
// reserved namespace and ends it with the leg it is, so this reads the record
// rather than guessing from prices.
func causeOf(orderID string) string {
	switch {
	case strings.HasSuffix(orderID, ":stop"):
		return "stop"
	case strings.HasSuffix(orderID, ":target"):
		return "target"
	default:
		return "order"
	}
}

// decimal is market's spelling, not a second one. Every quantity crosses as a
// whole count of its smallest unit, and the reader on the other side of this
// boundary is held to the same rule.
func decimal(v int64) string { return market.FormatInt(v) }

// project builds what the participant may see from what the session holds.
func project(s *session.Session, cursor, observations int, cfg session.Config, valuation session.Valuation, valueErr error) State {
	state := State{
		Subject:      cfg.SubjectID,
		Pacing:       cfg.Pacing.String(),
		Cursor:       cursor,
		Observations: observations,
		SessionOpen:  s.TradingSessionOpen(),
		SessionID:    string(s.OpenSessionID()),
		Money: &Money{
			BalanceCts: decimal(int64(valuation.BalanceCts)),
			EquityCts:  decimal(int64(valuation.EquityCts)),
		},

		// The streak the journal will record on the next decision. It is asked
		// of the session rather than derived, because OrderContext documents it
		// as what the trader knew and the screen is the only thing that can
		// make that true.
		ConsecutiveLosingTrades: s.ConsecutiveLosingTrades(),
		Fills:                   fillsOnScreen(s.Events()),
	}
	// A valuation that could not be taken leaves no money on the screen. It is
	// not a figure to be replaced by another one: the balance it used to fall
	// back to is money a participant holding a losing position does not have,
	// and it is a number they act on.
	if valueErr != nil {
		state.Money, state.NeedsRecovery = nil, valueErr.Error()
	}
	// And a session that has stopped is said last, because it is the more
	// fundamental of the two.
	if observed := s.LastObserved(); observed != 0 {
		state.ObservedSequence = market.FormatUint(observed)
	}
	if err := s.NeedsRecovery(); err != nil {
		state.NeedsRecovery = err.Error()
	}

	position, _ := s.Account().Position(cfg.Instrument)
	state.Position = Position{Symbol: cfg.Instrument.Symbol, NetQty: decimal(int64(position.NetQty))}

	state.Evaluation = Evaluation{State: s.Challenge().State().String()}
	if reason := s.Challenge().FailureReason(); reason != challenge.FailureNone {
		state.Evaluation.Reason = reason.String()
	}

	state.Working = make([]Order, 0, len(s.WorkingOrders()))
	for _, o := range s.WorkingOrders() {
		state.Working = append(state.Working, Order{
			ID: o.ID, Side: o.Side.String(), Type: o.Type.String(),
			Qty:        decimal(int64(o.Qty)),
			LimitPrice: decimal(int64(o.LimitPrice)),
			StopPrice:  decimal(int64(o.StopPrice)),
		})
	}

	state.Protection = make([]Protection, 0, len(s.PlannedProtections())+len(s.ActiveProtections()))
	for _, p := range s.PlannedProtections() {
		state.Protection = append(state.Protection, Protection{
			Status: "planned", EntryOrderID: p.EntryOrderID,
			StopPrice: decimal(int64(p.StopPrice)), TargetPrice: decimal(int64(p.TargetPrice)),
		})
	}
	for _, p := range s.ActiveProtections() {
		state.Protection = append(state.Protection, Protection{
			Status: "active", EpisodeID: strconv.FormatUint(p.EpisodeID, 10),
			StopPrice: decimal(int64(p.StopPrice)), TargetPrice: decimal(int64(p.TargetPrice)),
			ProtectedQty: decimal(int64(p.ProtectedQty)),
		})
	}
	return state
}

// withBook adds the observation the participant is looking at.
func (s State) withBook(q market.Quote, has bool) State {
	if !has {
		return s
	}
	s.Book = &Book{
		Time:    decimal(int64(q.Time)),
		Bid:     decimal(int64(q.Bid)),
		Ask:     decimal(int64(q.Ask)),
		BidSize: decimal(int64(q.BidSize)),
		AskSize: decimal(int64(q.AskSize)),
	}
	return s
}

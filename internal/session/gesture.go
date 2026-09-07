package session

import (
	"errors"
	"fmt"

	"praxis/internal/market"
)

// ErrGestureReused reports a human act the journal already holds.
//
// It is how a retry is told from a second decision. A caller that sees it must
// not conclude the command failed: it must look up what that gesture already
// committed and answer with that, because a lost response is a retry and not a
// new act.
var ErrGestureReused = errors.New("session: this gesture has already been recorded")

// GestureKind says which command a human act performed.
type GestureKind uint8

const (
	GestureSubmitOrder GestureKind = iota + 1
	GestureCancelOrder
	GestureReplaceProtection
	GestureWithdrawProtection
)

func (k GestureKind) String() string {
	switch k {
	case GestureSubmitOrder:
		return "submit order"
	case GestureCancelOrder:
		return "cancel order"
	case GestureReplaceProtection:
		return "replace protection"
	case GestureWithdrawProtection:
		return "withdraw protection"
	default:
		return "unspecified"
	}
}

// Gesture is what one human act commanded, reconstructed from the journal.
//
// A set of spent identifiers is not enough to answer a retry. It says the act
// happened; it does not say what it did, so it cannot tell a resent command
// from a different command sent under a name that was reused by mistake. The
// application loop needs both: the same gesture with the same command is
// already committed, and the same gesture with a different one is a conflict.
//
// Decided is the original stamp and is recovered rather than made again. The
// server is the clock, so a retry arrives at a different instant; stamping it
// afresh would record a decision at a moment the person did not decide
// anything, and comparing the stamps would make every retry a conflict.
type Gesture struct {
	Kind    GestureKind
	Decided Decision

	// Order, StopPrice and TargetPrice describe a submission. A protection
	// placed with an entry is part of the same act, so it is part of the
	// comparison: resending an entry with different levels is a different
	// command, not a retry.
	Order       market.Order
	StopPrice   market.Ticks
	TargetPrice market.Ticks

	// OrderID is the order a cancellation named.
	OrderID string

	// Ref is the protection a replacement or a withdrawal named, and
	// StopPrice and TargetPrice above carry a replacement's new levels.
	Ref ProtectionRef
}

// SameCommand reports whether two acts asked for the same thing. The stamp is
// deliberately not compared: a retry is the same command at a later instant,
// and that is exactly the case this exists to recognise.
func (g Gesture) SameCommand(other Gesture) bool {
	if g.Kind != other.Kind {
		return false
	}
	switch g.Kind {
	case GestureSubmitOrder:
		return g.Order == other.Order &&
			g.StopPrice == other.StopPrice && g.TargetPrice == other.TargetPrice
	case GestureCancelOrder:
		return g.OrderID == other.OrderID
	case GestureReplaceProtection:
		return g.Ref == other.Ref &&
			g.StopPrice == other.StopPrice && g.TargetPrice == other.TargetPrice
	case GestureWithdrawProtection:
		return g.Ref == other.Ref
	default:
		return false
	}
}

// gestureIndex is every human act a journal holds, in the order it holds them.
//
// It lives apart from the protection projection it grew inside: an act is not a
// protective level, and the registry was only there because protective legs
// were the second thing in the system to need a name that is never reused.
//
// The slice is the order they were taken in, so what a reader reports never
// depends on iteration order. The map answers membership for one identifier at
// a time and is never iterated to produce a result.
type gestureIndex struct {
	taken []Gesture
	byID  map[string]int
}

func (g *gestureIndex) init() {
	if g.byID == nil {
		g.byID = map[string]int{}
	}
}

// claim records an act as taken, refusing one the journal already holds.
func (g *gestureIndex) claim(act Gesture) error {
	g.init()
	id := act.Decided.GestureID
	if id == "" {
		return nil
	}
	if _, taken := g.byID[id]; taken {
		return fmt.Errorf("%w: %s", ErrGestureReused, id)
	}
	g.byID[id] = len(g.taken)
	g.taken = append(g.taken, act)
	return nil
}

// find is what a gesture already committed, for a caller answering a retry.
func (g *gestureIndex) find(id string) (Gesture, bool) {
	g.init()
	if at, ok := g.byID[id]; ok {
		return g.taken[at], true
	}
	return Gesture{}, false
}

func (g *gestureIndex) used(id string) bool {
	_, ok := g.find(id)
	return ok
}

// attachProtection folds the levels of a protection placed with an entry into
// the act that submitted it. They are one command and one batch, so the
// placement always follows its submission immediately.
func (g *gestureIndex) attachProtection(stop, target market.Ticks) {
	if n := len(g.taken); n > 0 && g.taken[n-1].Kind == GestureSubmitOrder {
		g.taken[n-1].StopPrice, g.taken[n-1].TargetPrice = stop, target
	}
}

func (g *gestureIndex) snapshot() []Gesture {
	out := make([]Gesture, len(g.taken))
	copy(out, g.taken)
	return out
}

func (g *gestureIndex) restore(taken []Gesture) {
	g.init()
	g.taken = g.taken[:0]
	for _, act := range taken {
		g.byID[act.Decided.GestureID] = len(g.taken)
		g.taken = append(g.taken, act)
	}
}

// gestureOf is the act an event records, and whether it records one at all.
func gestureOf(e Event) (Gesture, bool) {
	switch v := e.(type) {
	case OrderSubmitted:
		return Gesture{Kind: GestureSubmitOrder, Decided: v.Decided, Order: v.Order}, !v.Decided.IsZero()
	case OrderCancelled:
		if v.Reason != CancelledByTrader {
			return Gesture{}, false
		}
		return Gesture{Kind: GestureCancelOrder, Decided: v.Decided, OrderID: v.OrderID}, !v.Decided.IsZero()
	case ProtectionReplaced:
		return Gesture{
			Kind: GestureReplaceProtection, Decided: v.Decided, Ref: v.Ref,
			StopPrice: v.StopPrice, TargetPrice: v.TargetPrice,
		}, !v.Decided.IsZero()
	case ProtectionEnded:
		if v.Reason != ProtectionWithdrawnByTrader {
			return Gesture{}, false
		}
		return Gesture{Kind: GestureWithdrawProtection, Decided: v.Decided, Ref: v.Ref}, !v.Decided.IsZero()
	default:
		return Gesture{}, false
	}
}

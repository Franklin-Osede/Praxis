package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"praxis/internal/market"
	"praxis/internal/session"
)

// command is what a client asks for. One route with a tagged body rather than
// four routes, because the preamble every command needs — find the gesture,
// check the lease, compare, execute — would otherwise be written four times and
// the four would drift. A tagged payload is what an event is; this is the same
// shape one layer out.
type command struct {
	Kind  string `json:"kind"`
	Lease string `json:"lease"`

	// Gesture is the act's name, minted by the client as <Segment>:<Sequence>.
	// The segment is the one the server issued with the lease, and the sequence
	// is the client's counter within it.
	//
	// The client sends no clock. Both ends of an interval are stamped here,
	// when the request arrives, so the measurement is server receipt to server
	// receipt — and so that the one quantity this whole apparatus exists to
	// produce is not a figure the browser supplied and nothing can contradict.
	Gesture string `json:"gesture"`

	// An order. Qty, and the prices the type requires.
	OrderID    string `json:"orderId"`
	Side       string `json:"side"`
	Type       string `json:"type"`
	Qty        string `json:"qty"`
	LimitPrice string `json:"limitPrice"`
	StopPrice  string `json:"stopPrice"`

	// Protective levels placed with an entry, or the new ones for a change.
	ProtectionStop   string `json:"protectionStop"`
	ProtectionTarget string `json:"protectionTarget"`

	// The protection a change or a withdrawal names.
	RefKind      string `json:"refKind"`
	RefOrderID   string `json:"refOrderId"`
	RefEpisodeID string `json:"refEpisodeId"`
}

// The four human commands, tagged.
const (
	kindSubmitOrder        = "submit_order"
	kindCancelOrder        = "cancel_order"
	kindReplaceProtection  = "replace_protection"
	kindWithdrawProtection = "withdraw_protection"
)

// handleCommand runs one human command.
//
// Everything after reading the body happens inside one turn of the loop: the
// lease is checked there, the gesture is looked up there, and the command runs
// there. Two turns would let two concurrent retries of one gesture both find
// nothing and both execute — and the lease cannot serialise that, because a
// segment does not prove exclusive control and two tabs sharing a token can
// interleave. The loop is the only serialisation there is.
func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	var body command
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, refusal{ReasonUnreadable, err.Error()})
		return
	}

	var (
		state     State
		refusedBy error
	)
	if err := s.ask(func() {
		at, err := s.lease.stamp(body.Lease)
		if err != nil {
			refusedBy = err
			return
		}
		refusedBy = s.run(body, at)
		state = s.state()
	}); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, refusal{ReasonNeedsRecovery, err.Error()})
		return
	}
	if refusedBy != nil {
		s.refuse(w, refusedBy)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// run is the preamble and the switch, on the loop. It is one implementation for
// all four commands, which is the reason there is one route.
func (s *Server) run(body command, at session.Instant) error {
	decided, err := decisionOf(body, at)
	if err != nil {
		return err
	}
	asked, err := gestureOf(body, s.cfg.Instrument)
	if err != nil {
		return err
	}

	// A retry is recognised here and never reaches the kernel, which refuses a
	// repeated gesture without looking at what it committed. The same act asked
	// again is the state; a different act under a name already spent is the
	// conflict the kernel would have called a reuse.
	if held, taken := s.session.Gesture(decided.GestureID); taken {
		if held.SameCommand(asked) {
			return nil
		}
		return fmt.Errorf("%w: %s commanded %v, not %v",
			session.ErrGestureReused, decided.GestureID, held.Kind, asked.Kind)
	}

	switch body.Kind {
	case kindSubmitOrder:
		if asked.StopPrice == 0 && asked.TargetPrice == 0 {
			return s.session.SubmitOrder(asked.Order, decided)
		}
		return s.session.SubmitOrderWithProtection(asked.Order, asked.StopPrice, asked.TargetPrice, decided)
	case kindCancelOrder:
		return s.session.CancelOrder(asked.OrderID, decided)
	case kindReplaceProtection:
		return s.session.ReplaceProtection(asked.Ref, asked.StopPrice, asked.TargetPrice, decided)
	case kindWithdrawProtection:
		return s.session.CancelProtection(asked.Ref, decided)
	default:
		return fmt.Errorf("%w: %q is not a command", errUnknownKind, body.Kind)
	}
}

// decisionOf reads the act's stamp, and holds the identifier to the segment it
// names.
//
// The segment appears twice — in the identifier and on the decision — so it is
// checked rather than trusted, the same way Widened is recomputed rather than
// believed. The refusal then names what actually happened: an act belonging to
// a run this is not, instead of a reading out of order, which is what the clock
// would have reported for a client whose lease had been taken.
func decisionOf(body command, at session.Instant) (session.Decision, error) {
	if err := market.ValidIdentifier(body.Gesture); err != nil {
		return session.Decision{}, err
	}
	named, sequence, ok := strings.Cut(body.Gesture, ":")
	if !ok {
		return session.Decision{}, fmt.Errorf("%w: %q is not <segment>:<sequence>",
			errWrongSegment, body.Gesture)
	}
	// Both halves. Without a sequence every act in a run is one name, and the
	// second would be answered as a retry of the first — an order the person
	// asked for and never got, reported as success.
	if _, err := market.ParseUint(sequence); err != nil {
		return session.Decision{}, fmt.Errorf("%w: %q has no sequence: %v",
			errWrongSegment, body.Gesture, err)
	}
	if named != market.FormatUint(at.Segment) {
		return session.Decision{}, fmt.Errorf("%w: %q names run %s, and this lease is run %d",
			errWrongSegment, body.Gesture, named, at.Segment)
	}
	return session.Decision{
		GestureID:    body.Gesture,
		AtUTCNanos:   at.AtUTCNanos,
		Segment:      at.Segment,
		ElapsedNanos: at.ElapsedNanos,
	}, nil
}

// gestureOf is what the body asked for, as the act the register compares. It is
// built before the command runs so that a retry can be recognised by comparing
// what it asked for, not by re-running it.
// The instrument is the session's and never the client's: there is exactly one,
// and letting a body name it would let a body name a different one.
func gestureOf(body command, instrument market.Instrument) (session.Gesture, error) {
	prices := func() (stop, target market.Ticks, err error) {
		if stop, err = optionalTicks(body.ProtectionStop); err != nil {
			return 0, 0, err
		}
		target, err = optionalTicks(body.ProtectionTarget)
		return stop, target, err
	}

	switch body.Kind {
	case kindSubmitOrder:
		o, err := orderOf(body, instrument)
		if err != nil {
			return session.Gesture{}, err
		}
		stop, target, err := prices()
		if err != nil {
			return session.Gesture{}, err
		}
		return session.Gesture{
			Kind: session.GestureSubmitOrder, Order: o,
			StopPrice: stop, TargetPrice: target,
		}, nil
	case kindCancelOrder:
		if err := market.ValidIdentifier(body.OrderID); err != nil {
			return session.Gesture{}, err
		}
		return session.Gesture{Kind: session.GestureCancelOrder, OrderID: body.OrderID}, nil
	case kindReplaceProtection:
		ref, err := refOf(body)
		if err != nil {
			return session.Gesture{}, err
		}
		stop, target, err := prices()
		if err != nil {
			return session.Gesture{}, err
		}
		return session.Gesture{
			Kind: session.GestureReplaceProtection, Ref: ref,
			StopPrice: stop, TargetPrice: target,
		}, nil
	case kindWithdrawProtection:
		ref, err := refOf(body)
		if err != nil {
			return session.Gesture{}, err
		}
		return session.Gesture{Kind: session.GestureWithdrawProtection, Ref: ref}, nil
	default:
		return session.Gesture{}, fmt.Errorf("%w: %q is not a command", errUnknownKind, body.Kind)
	}
}

func orderOf(body command, instrument market.Instrument) (market.Order, error) {
	qty, err := market.ParseInt(body.Qty)
	if err != nil {
		return market.Order{}, err
	}
	limit, err := optionalTicks(body.LimitPrice)
	if err != nil {
		return market.Order{}, err
	}
	stop, err := optionalTicks(body.StopPrice)
	if err != nil {
		return market.Order{}, err
	}
	side, err := sideOf(body.Side)
	if err != nil {
		return market.Order{}, err
	}
	orderType, err := typeOf(body.Type)
	if err != nil {
		return market.Order{}, err
	}
	return market.Order{
		ID: body.OrderID, Instrument: instrument, Side: side, Type: orderType,
		Qty: market.Qty(qty), LimitPrice: limit, StopPrice: stop,
	}, nil
}

func refOf(body command) (session.ProtectionRef, error) {
	switch body.RefKind {
	case "entry":
		if err := market.ValidIdentifier(body.RefOrderID); err != nil {
			return session.ProtectionRef{}, err
		}
		return session.ProtectionRef{
			Kind: session.ProtectionRefEntry, OrderID: body.RefOrderID,
		}, nil
	case "episode":
		id, err := market.ParseUint(body.RefEpisodeID)
		if err != nil {
			return session.ProtectionRef{}, err
		}
		return session.ProtectionRef{Kind: session.ProtectionRefEpisode, EpisodeID: id}, nil
	default:
		return session.ProtectionRef{}, fmt.Errorf("%w: %q names neither an entry nor an episode",
			errUnknownKind, body.RefKind)
	}
}

// optionalTicks reads a price that a command may leave out. Absent is the empty
// string and not "0": zero is what the domain means by a level that is not set,
// and a client that sent it deliberately means the same thing.
func optionalTicks(s string) (market.Ticks, error) {
	if s == "" {
		return 0, nil
	}
	v, err := market.ParseInt(s)
	if err != nil {
		return 0, err
	}
	return market.Ticks(v), nil
}

func sideOf(s string) (market.Side, error) {
	switch s {
	case "buy":
		return market.SideBuy, nil
	case "sell":
		return market.SideSell, nil
	default:
		return 0, fmt.Errorf("%w: %q is not a side", errUnknownKind, s)
	}
}

func typeOf(s string) (market.OrderType, error) {
	switch s {
	case "market":
		return market.OrderTypeMarket, nil
	case "limit":
		return market.OrderTypeLimit, nil
	case "stop":
		return market.OrderTypeStop, nil
	default:
		return 0, fmt.Errorf("%w: %q is not an order type", errUnknownKind, s)
	}
}

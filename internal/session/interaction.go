package session

import (
	"errors"
	"fmt"
)

// Errors reported when a journal's record of the person at the controls does
// not describe something that could have happened.
var (
	// ErrInteractionStamp reports an event whose stamp and whose run disagree
	// about whether anybody was there: a scripted run recording somebody
	// acting, a traded one recording an act nobody timed, or something the log
	// itself required claiming a person decided it.
	ErrInteractionStamp = errors.New("session: the stamp and the run disagree about whether a person was there")

	// ErrInteractionOrder reports a chronology that cannot have happened: a
	// segment that went backwards, or a monotonic reading that decreased
	// inside one segment. The second is the one that matters — an interval is
	// computed by subtracting two readings in one segment, and a decreasing
	// pair produces a negative one.
	ErrInteractionOrder = errors.New("session: the record of interaction is not in the order it happened")
)

// malformedDecision reports a stamp that is neither wholly absent nor wholly
// present. The moment is half of it and the act that names it is the other: a
// stamp carrying a gesture and no segment reads as "nobody was there" while
// plainly recording that somebody was, and the half that survives is the half
// no interval can be computed from.
//
// It is the one definition. Decision.Malformed answers with it, and so does the
// clock, because two implementations of one rule is what this package refuses
// everywhere else.
func malformedDecision(at Instant, gestureID string) bool {
	return at.Malformed() || at.IsZero() != (gestureID == "")
}

// interactionClass says what an event's moment is, and therefore which rule
// governs it.
//
// The three are not degrees of the same thing. A person deciding and an
// interface confirming what it put on their screen are both interaction, and
// both belong to one chronology — but only the first is a decision, and only
// the first carries a gesture. Everything else in a journal is something the
// log itself required, and required things have no clock at all.
type interactionClass uint8

const (
	// classDerived is an event the events before it demanded: a fill, a
	// valuation, a position change, a leg cancelled by its sibling, an ending
	// a fill required. Nobody commanded it, so nobody timed it.
	classDerived interactionClass = iota

	// classHumanDecision is the head of one of the four human commands.
	classHumanDecision

	// classPresentation is an interface confirming that an observation reached
	// a person's screen. It is not a decision — it commands nothing and
	// carries no gesture — but it is the beginning of every interval the
	// decisions are measured over, so it is held to the same chronology.
	classPresentation
)

func (c interactionClass) String() string {
	switch c {
	case classHumanDecision:
		return "a decision"
	case classPresentation:
		return "a presentation"
	default:
		return "a derived event"
	}
}

// interactionOf is what an event records about the person at the controls: the
// class it belongs to, and the moment it carries.
//
// The classification reads the event's own reason field for the two kinds that
// can be either — a cancellation and an ending are a person's act only when
// they say they are. That is safe only because requireOwed pins the reason of
// every cancellation and ending the log demanded, against what the events
// before it require. The two checks are load-bearing together: without the
// second, relabelling a derived cancellation as the trader's would move it into
// the class that may carry a clock.
func interactionOf(e Event) (interactionClass, Instant, string) {
	human := func(d Decision) (interactionClass, Instant, string) {
		return classHumanDecision, d.instant(), d.GestureID
	}
	derived := func(d Decision) (interactionClass, Instant, string) {
		return classDerived, d.instant(), d.GestureID
	}
	switch v := e.(type) {
	case OrderSubmitted:
		return human(v.Decided)
	case ProtectionReplaced:
		return human(v.Decided)
	case OrderCancelled:
		if v.Reason != CancelledByTrader {
			return derived(v.Decided)
		}
		return human(v.Decided)
	case ProtectionEnded:
		if v.Reason != ProtectionWithdrawnByTrader {
			return derived(v.Decided)
		}
		return human(v.Decided)
	case ObservationPresented:
		// A presentation commands nothing, so it names no act. Its moment is
		// held to the chronology all the same, because it is the beginning of
		// every interval the decisions are measured over.
		return classPresentation, v.Presented, ""
	default:
		return classDerived, Instant{}, ""
	}
}

// interactionClock holds the chronology of everything a person did to a
// journal: which run of uninterrupted interaction the log has reached, and how
// far into it.
//
// It is one implementation with three callers — the live session, Replay and
// Verify — for the reason every shared projection here exists. It is the same
// rule whichever side asks, and the last time a rule about a stamp lived only
// on the reading side, the writing side committed journals its own readers
// refused: clean on disk, invalid to everything that could read them, and with
// no repair that could help, because nothing was damaged.
type interactionClock struct {
	// subjectID and pacing are the run's own claim about whether anybody was
	// there. pacingAgreesWithSubject holds them to each other at the door and
	// again in Replay, so either one answers the question; both are kept
	// because a diagnostic that can name the subject is worth more.
	subjectID string
	pacing    PacingMode

	segment uint64
	elapsed ElapsedNanos
}

// traded reports whether a person was at the controls of this run at all.
func (c interactionClock) traded() bool { return c.pacing != PacingScripted }

// Check refuses an event whose moment could not have happened. It changes
// nothing, so a caller may ask before it has decided to record anything and a
// refusal costs exactly nothing: no gesture spent, no reading advanced, no
// event written.
//
// That purity is the point of splitting it from Apply. A check that advanced
// the clock and was then followed by a later refusal would turn a command that
// left no events into one that silently moved the chronology — the hardest
// class of bug this codebase can have, because the journal would still look
// perfect and only the intervals would be wrong.
//
// What it does not check is the wall clock's order. A wall clock can
// legitimately move backwards: a time server corrects it, an operator sets it,
// a suspended machine resumes. Refusing a corrected clock would refuse a
// session that was entirely honest, which is the worse failure. The monotonic
// reading is what intervals are computed from, and that one is held to its
// discipline.
func (c interactionClock) Check(e Event) error {
	class, at, gestureID := interactionOf(e)
	return c.check(class, e.Header().Kind, at, gestureID)
}

// check is Check over a moment on its own, so a command can ask before it has
// built the event that will carry it. The door and the journal ask the same
// function rather than two functions that agree today.
func (c interactionClock) check(class interactionClass, kind Kind, at Instant, gestureID string) error {
	if err := c.checkShape(class, kind, at, gestureID); err != nil {
		return err
	}
	return c.checkOrder(class, at)
}

// checkShape is the half of Check that does not depend on the chronology: a
// moment wholly present or wholly absent, and one that belongs in this run at
// all. It is separate because a caller that must answer a retry before it
// consults the reading still has to refuse a malformed one — a retry carries
// the stamp it originally sent, which is legitimately behind the log by then,
// but nothing makes a half-written stamp acceptable.
func (c interactionClock) checkShape(class interactionClass, kind Kind, at Instant, gestureID string) error {
	// A presentation names no act, so it is only the moment. A decision is the
	// moment and the act together.
	malformed := at.Malformed()
	if class == classHumanDecision {
		malformed = malformedDecision(at, gestureID)
	}
	if malformed {
		return fmt.Errorf("%w: %v records part of a moment — %+v, gesture %q — and not the rest of it",
			ErrMalformedDecision, kind, at, gestureID)
	}

	switch class {
	case classDerived:
		// Nothing required by the log may claim a person behind it. Crediting
		// a derived event to a human clock would put a decision in the record
		// that nobody took.
		if !at.IsZero() {
			return fmt.Errorf("%w: %v: nobody commanded it, and it records a person deciding it",
				ErrInteractionStamp, kind)
		}
		return nil

	case classPresentation:
		// A scripted run has no interface and no screen. A presentation in one
		// is not an unstamped presentation — it is an event that does not
		// belong to that kind of run at all, so a zero stamp would not make it
		// honest.
		if !c.traded() {
			return fmt.Errorf("%w: %v: nothing is presented in a run nobody watched",
				ErrInteractionStamp, kind)
		}
		if at.IsZero() {
			return fmt.Errorf("%w: %v: %s was shown this, and it records no moment at which they were",
				ErrInteractionStamp, kind, c.subjectID)
		}

	case classHumanDecision:
		// Zero means no person was there. Establishing that made the converse
		// a rule worth holding: an interface that forgot to stamp a command —
		// or stamped three of the four — would produce a log asserting both
		// that somebody traded it and that nobody decided anything in it, and
		// nothing would notice until the analysis, by which time the timing
		// data for that pilot session no longer exists.
		if c.traded() && at.IsZero() {
			return fmt.Errorf("%w: %v: %s traded this journal, and this records no moment at which they decided it",
				ErrInteractionStamp, kind, c.subjectID)
		}
		if !c.traded() && !at.IsZero() {
			return fmt.Errorf("%w: %v: recorded as decided, and nobody is recorded as having traded this journal",
				ErrInteractionStamp, kind)
		}
	}

	return nil
}

// checkOrder is the half that does depend on the chronology: where the log has
// reached, and whether this moment can follow it.
func (c interactionClock) checkOrder(class interactionClass, at Instant) error {
	if at.IsZero() {
		return nil
	}
	// A segment is a run of uninterrupted interaction and only ever goes up.
	// An old one reappearing would be a stale tab interleaving its acts with a
	// resumed session's, which is the failure a single lease on the controls
	// exists to prevent and this is the record of that lease holding.
	if at.Segment < c.segment {
		return fmt.Errorf("%w: %v in segment %d, after segment %d",
			ErrInteractionOrder, class, at.Segment, c.segment)
	}
	// Within one segment the monotonic reading never goes back. Equality is
	// allowed: two things can happen at the same reading, and a latency of
	// zero is a real measurement rather than an impossible one. A new segment
	// may begin at any reading, because nothing carried across the
	// interruption that ended the last one.
	if at.Segment == c.segment && at.ElapsedNanos < c.elapsed {
		return fmt.Errorf("%w: %v %dns into segment %d, after %dns into it",
			ErrInteractionOrder, class, at.ElapsedNanos, at.Segment, c.elapsed)
	}
	return nil
}

// Apply advances the chronology past an event that has been recorded.
//
// Check guarantees it cannot fail, which is why it returns nothing: everything
// that could refuse this moment has already refused it. It is called only after
// the event is in the journal, so the reading the log holds and the reading the
// clock holds can never be two different things.
func (c *interactionClock) Apply(e Event) {
	_, at, _ := interactionOf(e)
	if at.IsZero() {
		return
	}
	c.segment, c.elapsed = at.Segment, at.ElapsedNanos
}

// checkDecision is the door every human command passes before anything of it
// decides: the act must be one the journal has not already recorded, and its
// moment must be one that could have happened.
//
// It changes nothing. A command refused here leaves no order, no gesture spent,
// no reading advanced and no event — which is what lets a client correct a
// stamp and send the same act again.
func (s *Session) checkDecision(kind Kind, d Decision) error {
	if err := s.checkGesture(d); err != nil {
		return err
	}
	return s.clock.check(classHumanDecision, kind, d.instant(), d.GestureID)
}

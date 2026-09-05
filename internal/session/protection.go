package session

import (
	"errors"
	"fmt"

	"praxis/internal/market"
)

// Errors reported when a protection command cannot describe something the
// session can do.
var (
	ErrProtectionEmpty      = errors.New("session: a protection with neither a stop nor a target protects nothing")
	ErrProtectionExists     = errors.New("session: this entry already has a protection")
	ErrNoSuchProtection     = errors.New("session: no protection with that reference")
	ErrProtectionNotPlanned = errors.New("session: that protection is no longer planned")
	ErrReservedNamespace    = errors.New("session: order identifiers beginning with praxis: belong to the system")
	ErrOrderIDReused        = errors.New("session: this order identifier has already been used")

	// ErrProtectionOutlivedEntry reports a journal in which an entry was
	// cancelled and the protection planned with it was not ended. The plan
	// names an order that is gone, so a reader replaying that log would carry
	// forward cover that can never be placed.
	ErrProtectionOutlivedEntry = errors.New("session: a planned protection outlived its entry")
)

// reservedPrefix is the namespace the system draws protective order names
// from. A trader cannot submit an order inside it, so a derived name can never
// collide with one somebody chose.
const reservedPrefix = "praxis:"

// plannedProtection is a protection whose entry has not filled.
//
// Slice one carries only this state. Activation, protected quantities and
// execution are the next slices, and nothing here should need them.
type plannedProtection struct {
	entryOrderID  string
	stopPrice     market.Ticks
	targetPrice   market.Ticks
	stopOrderID   string
	targetOrderID string
}

// protectionProjection derives the protections a journal describes.
//
// Like the episode projection, one implementation serves the live session,
// Verify and Replay, so a journal and the checks on it cannot disagree about
// what was protected.
type protectionProjection struct {
	// planned is an ordered slice: the order protections are visited is part
	// of what a reader sees, and must never depend on iteration order.
	planned []plannedProtection

	// usedOrderIDs answers membership only and is never iterated to produce a
	// result, which is why a map is the right structure here.
	usedOrderIDs map[string]bool

	// awaitingEnd is the entry whose cancellation left a plan behind. The very
	// next event must be that plan's ending: they are one decision, so a
	// reader that allowed anything between them would be allowing a journal
	// the producer cannot write. An identifier is never empty, so the empty
	// string means there is nothing owed.
	awaitingEnd string
}

func (p *protectionProjection) init() {
	if p.usedOrderIDs == nil {
		p.usedOrderIDs = map[string]bool{}
	}
}

// claim records an identifier as used forever.
//
// An identifier is never reused anywhere in a journal, not merely while its
// order is working. A finished order leaves its name attributable, and a later
// order taking it would make grouping fills by decision ambiguous again —
// which is the one thing the identifier exists for.
func (p *protectionProjection) claim(ids ...string) error {
	p.init()
	for _, id := range ids {
		if id == "" {
			continue
		}
		if p.usedOrderIDs[id] {
			return fmt.Errorf("%w: %s", ErrOrderIDReused, id)
		}
	}
	for _, id := range ids {
		if id != "" {
			p.usedOrderIDs[id] = true
		}
	}
	return nil
}

func (p *protectionProjection) used(id string) bool {
	p.init()
	return p.usedOrderIDs[id]
}

func (p *protectionProjection) indexOfEntry(orderID string) int {
	for n, planned := range p.planned {
		if planned.entryOrderID == orderID {
			return n
		}
	}
	return -1
}

// plannedFor returns the protection planned against an entry, if there is one.
func (p *protectionProjection) plannedFor(orderID string) (plannedProtection, bool) {
	if at := p.indexOfEntry(orderID); at >= 0 {
		return p.planned[at], true
	}
	return plannedProtection{}, false
}

// applyPlaced folds a placement into the projection.
func (p *protectionProjection) applyPlaced(e ProtectionPlaced) error {
	if err := p.claim(e.StopOrderID, e.TargetOrderID); err != nil {
		return err
	}
	if p.indexOfEntry(e.EntryOrderID) >= 0 {
		return fmt.Errorf("%w: %s", ErrProtectionExists, e.EntryOrderID)
	}
	p.planned = append(p.planned, plannedProtection{
		entryOrderID: e.EntryOrderID,
		stopPrice:    e.StopPrice, targetPrice: e.TargetPrice,
		stopOrderID: e.StopOrderID, targetOrderID: e.TargetOrderID,
	})
	return nil
}

// applyReplaced folds a change into the projection.
func (p *protectionProjection) applyReplaced(e ProtectionReplaced) error {
	if e.Ref.Kind != ProtectionRefEntry {
		return fmt.Errorf("%w: only a planned protection exists yet", ErrNoSuchProtection)
	}
	at := p.indexOfEntry(e.Ref.OrderID)
	if at < 0 {
		return fmt.Errorf("%w: %s", ErrNoSuchProtection, e.Ref.OrderID)
	}
	// Only a name that did not exist before is newly claimed; a level that
	// survived keeps the one it already had.
	fresh := []string{}
	if e.StopOrderID != "" && e.StopOrderID != p.planned[at].stopOrderID {
		fresh = append(fresh, e.StopOrderID)
	}
	if e.TargetOrderID != "" && e.TargetOrderID != p.planned[at].targetOrderID {
		fresh = append(fresh, e.TargetOrderID)
	}
	if err := p.claim(fresh...); err != nil {
		return err
	}

	p.planned[at].stopPrice, p.planned[at].targetPrice = e.StopPrice, e.TargetPrice
	p.planned[at].stopOrderID, p.planned[at].targetOrderID = e.StopOrderID, e.TargetOrderID
	return nil
}

// applyEnded removes a protection.
func (p *protectionProjection) applyEnded(e ProtectionEnded) error {
	if e.Ref.Kind != ProtectionRefEntry {
		return fmt.Errorf("%w: only a planned protection exists yet", ErrNoSuchProtection)
	}
	at := p.indexOfEntry(e.Ref.OrderID)
	if at < 0 {
		return fmt.Errorf("%w: %s", ErrNoSuchProtection, e.Ref.OrderID)
	}
	p.planned = append(p.planned[:at], p.planned[at+1:]...)
	return nil
}

// entryGone notes that an order has been cancelled. If it carried a plan that
// never activated, the ending of that plan is now owed.
//
// A plan that has activated is not touched: the episode governs it, and the
// entry's remainder disappearing means nothing to what it already opened.
func (p *protectionProjection) entryGone(orderID string) {
	if p.indexOfEntry(orderID) >= 0 {
		p.awaitingEnd = orderID
	}
}

// requireEnding refuses any event standing between an entry's cancellation and
// the ending of the plan it left behind.
func (p *protectionProjection) requireEnding(e Event) error {
	owed := p.awaitingEnd
	if owed == "" {
		return nil
	}
	// This first branch is for the diagnostic alone: an event of another kind
	// would fail the reference check below anyway, on a zero value, and say so
	// far less usefully. Naming what actually stood in the way is what a reader
	// of a rejected journal needs.
	ended, ok := e.(ProtectionEnded)
	if !ok {
		return fmt.Errorf("%w: %s was cancelled and a %v followed instead of the ending",
			ErrProtectionOutlivedEntry, owed, e.Header().Kind)
	}
	if ended.Ref.Kind != ProtectionRefEntry || ended.Ref.OrderID != owed {
		return fmt.Errorf("%w: %s was cancelled and the ending that followed names %+v",
			ErrProtectionOutlivedEntry, owed, ended.Ref)
	}
	if ended.Reason != ProtectionEntryCancelled {
		return fmt.Errorf("%w: %s was cancelled and its plan ends with reason %v",
			ErrProtectionOutlivedEntry, owed, ended.Reason)
	}
	p.awaitingEnd = ""
	return nil
}

// settled reports a stream that stopped owing an ending it never wrote.
func (p *protectionProjection) settled() error {
	if p.awaitingEnd == "" {
		return nil
	}
	return fmt.Errorf("%w: %s was cancelled and the log ends", ErrProtectionOutlivedEntry, p.awaitingEnd)
}

// PlannedProtection is what a reader outside the package sees.
type PlannedProtection struct {
	EntryOrderID  string
	StopPrice     market.Ticks
	TargetPrice   market.Ticks
	StopOrderID   string
	TargetOrderID string
}

func (p *protectionProjection) snapshot() []PlannedProtection {
	out := make([]PlannedProtection, 0, len(p.planned))
	for _, planned := range p.planned {
		out = append(out, PlannedProtection{
			EntryOrderID: planned.entryOrderID,
			StopPrice:    planned.stopPrice, TargetPrice: planned.targetPrice,
			StopOrderID: planned.stopOrderID, TargetOrderID: planned.targetOrderID,
		})
	}
	return out
}

func (p *protectionProjection) restore(planned []PlannedProtection, used []string) {
	p.init()
	p.planned = p.planned[:0]
	for _, s := range planned {
		p.planned = append(p.planned, plannedProtection{
			entryOrderID: s.EntryOrderID,
			stopPrice:    s.StopPrice, targetPrice: s.TargetPrice,
			stopOrderID: s.StopOrderID, targetOrderID: s.TargetOrderID,
		})
	}
	for _, id := range used {
		p.usedOrderIDs[id] = true
	}
}

// usedIdentifiers lists every name a journal has spent, sorted so that two
// runs report it identically.
func (p *protectionProjection) usedIdentifiers() []string {
	p.init()
	out := make([]string, 0, len(p.usedOrderIDs))
	for id := range p.usedOrderIDs {
		out = append(out, id)
	}
	sortStrings(out)
	return out
}

// sortStrings is insertion sort: the sets are small, and depending on a
// library's ordering guarantees for something a journal is compared on is not
// worth the import.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// legNames are the identifiers a protection event's own sequence gives its
// levels. A level that is not set reserves nothing.
func legNames(sequence uint64, stop, target market.Ticks) (stopID, targetID string) {
	if stop != 0 {
		stopID = fmt.Sprintf("%s%d:stop", reservedPrefix, sequence)
	}
	if target != 0 {
		targetID = fmt.Sprintf("%s%d:target", reservedPrefix, sequence)
	}
	return stopID, targetID
}

// validLevels refuses a protection that protects nothing, and a level that is
// not a price.
func validLevels(stop, target market.Ticks) error {
	if stop == 0 && target == 0 {
		return ErrProtectionEmpty
	}
	if stop < 0 || target < 0 {
		return market.ErrNonPositivePrice
	}
	return nil
}

// widened reports whether a stop moved away from the entry. Replacing a level
// from nothing is placing it, not widening it, and a long is protected below
// while a short is protected above.
func widened(long bool, previous, next market.Ticks) bool {
	if previous == 0 || next == 0 {
		return false
	}
	if long {
		return next < previous
	}
	return next > previous
}

// SubmitOrderWithProtection submits an entry and the levels planned with it,
// as one command and therefore one durable batch.
//
// They are one decision. Recording them separately would allow a journal in
// which the entry exists and its protection does not, which is a state the
// trader never chose and which the log would have no way to explain.
func (s *Session) SubmitOrderWithProtection(o market.Order, stop, target market.Ticks) error {
	return s.command(func() error { return s.submitOrderWithProtection(o, stop, target) })
}

// The protection is recorded between the decision and its first fill, which is
// why the entry is prepared, recorded and executed in three steps rather than
// submitted whole.
//
// A plan written after the fill could not be bound to it. The episode would
// already have opened against a projection that had never heard of the plan,
// and the link between the two would have to be inferred backwards from a
// later event — which is guessing at causation from ordering, the thing a
// journal exists to make unnecessary.
func (s *Session) submitOrderWithProtection(o market.Order, stop, target market.Ticks) error {
	// Every refusal happens before anything is recorded, so a rejected command
	// leaves no order, no protection, no reserved name, no counter moved and
	// no event.
	if err := validLevels(stop, target); err != nil {
		return err
	}
	// The order's own faults are reported before the protection's. A duplicate
	// identifier is the more fundamental one: the protection exists only
	// because the order does, and naming the derived fault would send a reader
	// looking for a protection that was never the problem.
	prepared, err := s.prepareOrder(o)
	if err != nil {
		return err
	}
	if err := s.recordOrder(prepared); err != nil {
		return err
	}
	if err := s.placeProtection(prepared.at, o.ID, stop, target); err != nil {
		return err
	}
	if err := s.executeOrder(prepared); err != nil {
		return err
	}
	return s.revalue(prepared.at)
}

// placeProtection records the levels planned with an entry.
//
// The names come from the sequence the journal actually assigns this event,
// taken inside the builder rather than guessed at by counting how many events
// a command ought to have produced first.
func (s *Session) placeProtection(at market.LogicalTime, entryOrderID string, stop, target market.Ticks) error {
	var placed ProtectionPlaced
	if err := s.record(at, KindProtectionPlaced, func(e Envelope) Event {
		stopID, targetID := legNames(e.Sequence, stop, target)
		placed = ProtectionPlaced{
			Envelope: e, EntryOrderID: entryOrderID,
			StopPrice: stop, TargetPrice: target,
			StopOrderID: stopID, TargetOrderID: targetID,
		}
		return placed
	}); err != nil {
		return err
	}
	return s.protections.applyPlaced(placed)
}

// ReplaceProtection changes the levels of a protection that exists.
func (s *Session) ReplaceProtection(ref ProtectionRef, stop, target market.Ticks) error {
	return s.command(func() error { return s.replaceProtection(ref, stop, target) })
}

func (s *Session) replaceProtection(ref ProtectionRef, stop, target market.Ticks) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	// Replacing both levels with nothing is withdrawing the protection, and
	// must be said as that: CancelProtection names the decision it was.
	if err := validLevels(stop, target); err != nil {
		return err
	}
	planned, at, err := s.plannedForCommand(ref)
	if err != nil {
		return err
	}

	long, err := s.entryOpensLong(planned.entryOrderID)
	if err != nil {
		return err
	}

	var replaced ProtectionReplaced
	if err := s.record(at, KindProtectionReplaced, func(e Envelope) Event {
		stopID, targetID := planned.stopOrderID, planned.targetOrderID
		fresh, freshTarget := legNames(e.Sequence, stop, target)
		switch {
		case stop == 0:
			stopID = ""
		case stopID == "":
			stopID = fresh
		}
		switch {
		case target == 0:
			targetID = ""
		case targetID == "":
			targetID = freshTarget
		}
		replaced = ProtectionReplaced{
			Envelope: e, Ref: ref,
			PreviousStopPrice: planned.stopPrice, PreviousTargetPrice: planned.targetPrice,
			StopPrice: stop, TargetPrice: target,
			StopOrderID: stopID, TargetOrderID: targetID,
			Widened: widened(long, planned.stopPrice, stop),
		}
		return replaced
	}); err != nil {
		return err
	}
	return s.protections.applyReplaced(replaced)
}

// CancelProtection withdraws a protection, leaving its entry alone.
func (s *Session) CancelProtection(ref ProtectionRef) error {
	return s.command(func() error { return s.endProtection(ref, ProtectionWithdrawnByTrader) })
}

func (s *Session) endProtection(ref ProtectionRef, reason ProtectionEndReason) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	planned, at, err := s.plannedForCommand(ref)
	if err != nil {
		return err
	}

	return s.recordProtectionEnded(at, planned, ref, reason)
}

// endPlanFor ends the protection planned against an entry that no longer
// exists, and does nothing when there is none.
//
// A plan cannot outlive its entry. It names an order that is gone, so nothing
// could ever activate it, and until it was ended the session went on reporting
// it — cover the trader would read as theirs and does not have.
//
// It asks the projection rather than assuming, because once a plan activates
// the episode governs it and the entry's disappearance means nothing: a
// partially filled entry whose remainder is cancelled keeps protecting what it
// opened.
func (s *Session) endPlanFor(at market.LogicalTime, entryOrderID string) error {
	planned, ok := s.protections.plannedFor(entryOrderID)
	if !ok {
		return nil
	}
	ref := ProtectionRef{Kind: ProtectionRefEntry, OrderID: entryOrderID}
	return s.recordProtectionEnded(at, planned, ref, ProtectionEntryCancelled)
}

// recordProtectionEnded writes an ending and folds it in. Its caller has
// already resolved the protection and decided the command may proceed.
func (s *Session) recordProtectionEnded(at market.LogicalTime, planned plannedProtection, ref ProtectionRef, reason ProtectionEndReason) error {
	ended := ProtectionEnded{
		Ref: ref, StopPrice: planned.stopPrice, TargetPrice: planned.targetPrice,
		Reason: reason,
	}
	if err := s.record(at, KindProtectionEnded, func(e Envelope) Event {
		ended.Envelope = e
		return ended
	}); err != nil {
		return err
	}
	return s.protections.applyEnded(ended)
}

// plannedForCommand resolves a reference and refuses everything a protection
// command cannot act on, before anything is recorded.
func (s *Session) plannedForCommand(ref ProtectionRef) (plannedProtection, market.LogicalTime, error) {
	if !s.sessionOpen {
		return plannedProtection{}, 0, ErrNoSessionOpen
	}
	if s.ended() {
		return plannedProtection{}, 0, ErrChallengeEnded
	}
	if !s.hasQuote {
		return plannedProtection{}, 0, ErrNoMarketObserved
	}
	if ref.Kind == ProtectionRefEpisode {
		// Activation is the next slice. Saying so is better than pretending a
		// reference works when nothing can yet produce it.
		return plannedProtection{}, 0, fmt.Errorf("%w: no protection is active yet", ErrProtectionNotPlanned)
	}
	planned, ok := s.protections.plannedFor(ref.OrderID)
	if !ok {
		return plannedProtection{}, 0, fmt.Errorf("%w: %s", ErrNoSuchProtection, ref.OrderID)
	}
	at := s.lastQuote.Time
	if err := s.journal.ValidateNext(at, s.sequence+1); err != nil {
		return plannedProtection{}, 0, err
	}
	return planned, at, nil
}

// entryOpensLong reports the direction the entry would open, which is what
// makes a stop's move away from it a widening.
func (s *Session) entryOpensLong(orderID string) (bool, error) {
	for _, w := range s.working {
		if w.order.ID == orderID {
			return w.order.Side == market.SideBuy, nil
		}
	}
	return false, fmt.Errorf("%w: %s is not waiting", ErrNoSuchOrder, orderID)
}

// PlannedProtections returns the protections whose entries have not filled, in
// the order they were placed.
func (s *Session) PlannedProtections() []PlannedProtection { return s.protections.snapshot() }

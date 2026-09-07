package session

import (
	"errors"
	"fmt"

	"praxis/internal/market"
	"praxis/internal/portfolio"
)

// Errors reported when a protection command cannot describe something the
// session can do.
var (
	ErrProtectionEmpty    = errors.New("session: a protection with neither a stop nor a target protects nothing")
	ErrProtectionInverted = errors.New("session: the stop and the target are on the wrong sides of each other")
	ErrProtectionExists   = errors.New("session: this entry already has a protection")
	ErrNoSuchProtection   = errors.New("session: no protection with that reference")
	ErrReservedNamespace  = errors.New("session: order identifiers beginning with praxis: belong to the system")
	ErrOrderIDReused      = errors.New("session: this order identifier has already been used")

	// ErrMalformedDecision reports a stamp that is neither wholly absent nor
	// wholly present, which would read as nobody having been there while
	// plainly recording that somebody was.
	ErrMalformedDecision = errors.New("session: a decision records part of itself and not the rest")

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

// activeProtection is a protection bound to an episode, which is what a plan
// becomes once a fill has opened the exposure it was placed to cover.
//
// protectedQty is today always the episode's net exposure, because exactly one
// protection governs an episode and it covers the whole of it: every path that
// moves one moves the other. It is a field rather than a question asked of the
// account because the projection derives protection from events alone and owns
// no account, and it is pinned to abs(NetQty) by a test rather than assumed to
// agree. See ADR-014.
type activeProtection struct {
	episodeID uint64
	symbol    string

	stopPrice     market.Ticks
	targetPrice   market.Ticks
	stopOrderID   string
	targetOrderID string

	protectedQty market.Qty
	long         bool
}

// owedEvent is a fact the events so far require next, in order.
//
// The session records them; Replay and Verify recompute them and demand the
// journal holds exactly these, in this order, immediately. It is the same
// double entry the position changes already have: a derived fact is recomputed
// from what caused it rather than believed.
//
// Protection produces short tails — a leg's remainder cancelled, the sibling
// cancelled with it, the aggregate ended — so an owing is either a
// cancellation or an ending.
type owedEvent struct {
	kind Kind

	// An owed cancellation.
	orderID      string
	remainingQty market.Qty
	cancelReason CancelReason

	// An owed ending.
	ref         ProtectionRef
	stopPrice   market.Ticks
	targetPrice market.Ticks
	endReason   ProtectionEndReason

	// checkReason says the owing knows why an aggregate ended. Verify can see
	// that a closing episode's protection must end without seeing the fill,
	// and whether the fill reversed is the only thing the reason turns on.
	checkReason bool
}

func owedCancel(orderID string, remaining market.Qty, reason CancelReason) owedEvent {
	return owedEvent{
		kind: KindOrderCancelled, orderID: orderID,
		remainingQty: remaining, cancelReason: reason,
	}
}

func owedEnd(ref ProtectionRef, stop, target market.Ticks, reason ProtectionEndReason, checkReason bool) owedEvent {
	return owedEvent{
		kind: KindProtectionEnded, ref: ref,
		stopPrice: stop, targetPrice: target,
		endReason: reason, checkReason: checkReason,
	}
}

func entryReference(orderID string) ProtectionRef {
	return ProtectionRef{Kind: ProtectionRefEntry, OrderID: orderID}
}

func episodeReference(id uint64) ProtectionRef {
	return ProtectionRef{Kind: ProtectionRefEpisode, EpisodeID: id}
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

	// active holds the protections bound to an episode, ordered for the same
	// reason as planned.
	active []activeProtection

	// usedOrderIDs answers membership only and is never iterated to produce a
	// result, which is why a map is the right structure here.
	usedOrderIDs map[string]bool

	// owed holds the events the facts so far require, in order. The very next
	// event must be the first of them: a consequence and the fact that caused
	// it are one decision, so a reader that allowed anything between them would
	// be allowing a journal the producer cannot write.
	owed []owedEvent
}

func (p *protectionProjection) init() {
	if p.usedOrderIDs == nil {
		p.usedOrderIDs = map[string]bool{}
	}
}

// checkGesture refuses an act the journal already holds, and one the record
// could not write down, before anything is recorded.
//
// A repeated gesture is a retry — a lost response, a double click, a reloaded
// tab — and the caller answers "already committed" with the state that commit
// produced, rather than recording a second decision. Telling those two apart is
// the whole reason the act has a name of its own.
func (s *Session) checkGesture(d Decision) error {
	if d.Malformed() {
		return fmt.Errorf("%w: %+v", ErrMalformedDecision, d)
	}
	if d.IsZero() {
		return nil
	}
	if err := market.ValidIdentifier(d.GestureID); err != nil {
		return err
	}
	if s.gestures.used(d.GestureID) {
		return fmt.Errorf("%w: %s", ErrGestureReused, d.GestureID)
	}
	return nil
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

func (p *protectionProjection) indexOfEpisode(id uint64) int {
	for n, a := range p.active {
		if a.episodeID == id {
			return n
		}
	}
	return -1
}

// activeFor returns the protection bound to an episode, if there is one. Only
// one ever is: two plans over one position would be protection per lot, which
// ADR-013 rejected.
func (p *protectionProjection) activeFor(id uint64) (activeProtection, bool) {
	if at := p.indexOfEpisode(id); at >= 0 {
		return p.active[at], true
	}
	return activeProtection{}, false
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

// applyReplaced folds a change into the projection, whichever kind of reference
// names the protection.
func (p *protectionProjection) applyReplaced(e ProtectionReplaced) error {
	var stopID, targetID *string
	var stopPrice, targetPrice *market.Ticks
	if e.Ref.Kind == ProtectionRefEpisode {
		at := p.indexOfEpisode(e.Ref.EpisodeID)
		if at < 0 {
			return fmt.Errorf("%w: episode %d", ErrNoSuchProtection, e.Ref.EpisodeID)
		}
		stopID, targetID = &p.active[at].stopOrderID, &p.active[at].targetOrderID
		stopPrice, targetPrice = &p.active[at].stopPrice, &p.active[at].targetPrice
	} else {
		at := p.indexOfEntry(e.Ref.OrderID)
		if at < 0 {
			return fmt.Errorf("%w: %s", ErrNoSuchProtection, e.Ref.OrderID)
		}
		stopID, targetID = &p.planned[at].stopOrderID, &p.planned[at].targetOrderID
		stopPrice, targetPrice = &p.planned[at].stopPrice, &p.planned[at].targetPrice
	}

	// Only a name that did not exist before is newly claimed; a level that
	// survived keeps the one it already had.
	fresh := []string{}
	if e.StopOrderID != "" && e.StopOrderID != *stopID {
		fresh = append(fresh, e.StopOrderID)
	}
	if e.TargetOrderID != "" && e.TargetOrderID != *targetID {
		fresh = append(fresh, e.TargetOrderID)
	}
	if err := p.claim(fresh...); err != nil {
		return err
	}

	*stopPrice, *targetPrice = e.StopPrice, e.TargetPrice
	*stopID, *targetID = e.StopOrderID, e.TargetOrderID
	return nil
}

// levelsFor is the protection a reference names, as the levels a reader checks
// a recorded change or ending against.
func (p *protectionProjection) levelsFor(ref ProtectionRef) (protectionLevels, bool) {
	if ref.Kind == ProtectionRefEpisode {
		active, ok := p.activeFor(ref.EpisodeID)
		if !ok {
			return protectionLevels{}, false
		}
		return protectionLevels{
			stopPrice: active.stopPrice, targetPrice: active.targetPrice,
			stopOrderID: active.stopOrderID, targetOrderID: active.targetOrderID,
			long: active.long,
		}, true
	}
	planned, ok := p.plannedFor(ref.OrderID)
	if !ok {
		return protectionLevels{}, false
	}
	return protectionLevels{
		stopPrice: planned.stopPrice, targetPrice: planned.targetPrice,
		stopOrderID: planned.stopOrderID, targetOrderID: planned.targetOrderID,
	}, true
}

// applyEnded removes a protection, whichever kind of reference names it.
func (p *protectionProjection) applyEnded(e ProtectionEnded) error {
	if e.Ref.Kind == ProtectionRefEpisode {
		at := p.indexOfEpisode(e.Ref.EpisodeID)
		if at < 0 {
			return fmt.Errorf("%w: episode %d", ErrNoSuchProtection, e.Ref.EpisodeID)
		}
		p.active = append(p.active[:at], p.active[at+1:]...)
		return nil
	}
	at := p.indexOfEntry(e.Ref.OrderID)
	if at < 0 {
		return fmt.Errorf("%w: %s", ErrNoSuchProtection, e.Ref.OrderID)
	}
	p.planned = append(p.planned[:at], p.planned[at+1:]...)
	return nil
}

// consequencesOf is what a position change requires to be recorded next, in
// order. It changes nothing: a leg goes when an OrderCancelled says so and an
// aggregate goes when a ProtectionEnded says so, and both are events.
//
// flip says this close is the first half of a reversal — the same fill opens
// the other side. It is knowable only from the whole effect of a fill, which is
// why the effect is assembled before any of it is written down. Deciding on the
// close alone would end the plan as having opened no exposure, a moment before
// the exposure it opens.
//
// knowFlip says the caller can tell a reversal from an exit. Verify holds the
// events and not the fills, so it cannot; it is given the same list with the
// end reason unchecked and without the arriving plan's ending, which is a
// prefix of what a caller that knows demands.
func (p *protectionProjection) consequencesOf(
	fillOrderID string, episodeID uint64, change portfolio.PositionEvent,
	netQty market.Qty, flip, knowFlip bool,
) []owedEvent {
	var owed []owedEvent
	endPlan := func(reason ProtectionEndReason) {
		if planned, ok := p.plannedFor(fillOrderID); ok {
			owed = append(owed, owedEnd(entryReference(fillOrderID),
				planned.stopPrice, planned.targetPrice, reason, true))
		}
	}

	active, hasActive := p.activeFor(episodeID)
	// A fill from one of the protection's own legs is the protection working,
	// which is a different fact from the position going away underneath it.
	protective := hasActive && fillOrderID != "" &&
		(fillOrderID == active.stopOrderID || fillOrderID == active.targetOrderID)

	switch change.Kind {
	case portfolio.PositionClosed:
		if hasActive {
			// Every leg still standing goes. The one that executed has no
			// remainder to cancel: it filled the position away.
			reason := CancelledPositionClosed
			if protective {
				reason = CancelledByOCO
			}
			for _, leg := range []string{active.stopOrderID, active.targetOrderID} {
				if leg != "" && leg != fillOrderID {
					owed = append(owed, owedCancel(leg, active.protectedQty, reason))
				}
			}

			endReason, checkReason := ProtectionPositionClosed, true
			switch {
			case protective:
				endReason = ProtectionExecuted
			case flip:
				endReason = ProtectionFlipped
			case !knowFlip:
				checkReason = false
			}
			owed = append(owed, owedEnd(episodeReference(episodeID),
				active.stopPrice, active.targetPrice, endReason, checkReason))
		}
		// A plan on the closing order opened nothing — unless the same fill is
		// about to open the other side, which the plan then covers. A caller
		// that cannot tell demands nothing here rather than guessing.
		if knowFlip && !flip {
			endPlan(ProtectionDidNotOpenExposure)
		}

	case portfolio.PositionReduced:
		// A stop that reached its level has triggered and cannot untrigger, so
		// what the book could not fill does not go back to waiting. A target
		// is a limit and keeps waiting over what is left.
		if protective && fillOrderID == active.stopOrderID {
			owed = append(owed, owedCancel(active.stopOrderID, absQty(netQty), CancelledUnfillableRemainder))
		}
		endPlan(ProtectionDidNotOpenExposure)

	case portfolio.PositionIncreased:
		// A plan arriving at an episode that already has cover does not
		// silently replace it and is not kept beside it: it is ended, said out
		// loud, and the protection already there grows to the new exposure.
		if hasActive {
			endPlan(ProtectionAlreadyActive)
		}
	}
	return owed
}

// bind folds a position change into the protection state, once its endings have
// been recorded. It never removes a protection, so a reader holding the events
// but not the fills reaches the same state as one holding both.
func (p *protectionProjection) bind(entryOrderID string, episodeID uint64, change portfolio.PositionEvent, netQty market.Qty) {
	switch change.Kind {
	case portfolio.PositionOpened, portfolio.PositionIncreased:
		// Protection binds to the episode, and the episode grew. The quantity
		// covered is the whole net exposure and not the fill that arrived: a
		// plan activating on an addition to a position that had none covers
		// what is there, not what was just added.
		if at := p.indexOfEpisode(episodeID); at >= 0 {
			p.active[at].protectedQty = absQty(netQty)
			return
		}
		at := p.indexOfEntry(entryOrderID)
		if at < 0 {
			return
		}
		planned := p.planned[at]
		p.planned = append(p.planned[:at], p.planned[at+1:]...)
		p.active = append(p.active, activeProtection{
			episodeID: episodeID, symbol: change.Instrument.Symbol,
			stopPrice: planned.stopPrice, targetPrice: planned.targetPrice,
			stopOrderID: planned.stopOrderID, targetOrderID: planned.targetOrderID,
			protectedQty: absQty(netQty), long: netQty > 0,
		})
	case portfolio.PositionReduced:
		// Protection can never close contracts that are no longer there.
		if at := p.indexOfEpisode(episodeID); at >= 0 {
			p.active[at].protectedQty = absQty(netQty)
		}
	case portfolio.PositionClosed:
		// The episode is gone and an ending has already removed its cover.
	}
}

// legCancelled removes a leg from whichever protection owns it. A level that is
// gone is zero, which is what the rest of the domain already means by an unset
// price, so nothing offers it an observation again.
func (p *protectionProjection) legCancelled(orderID string) {
	if orderID == "" {
		return
	}
	for n := range p.active {
		switch orderID {
		case p.active[n].stopOrderID:
			p.active[n].stopPrice, p.active[n].stopOrderID = 0, ""
			return
		case p.active[n].targetOrderID:
			p.active[n].targetPrice, p.active[n].targetOrderID = 0, ""
			return
		}
	}
}

// legNamed finds the protection an identifier is a leg of, and which leg.
func (p *protectionProjection) legNamed(orderID string) (activeProtection, legKind, bool) {
	if orderID == "" {
		return activeProtection{}, 0, false
	}
	for _, a := range p.active {
		switch orderID {
		case a.stopOrderID:
			return a, legStop, true
		case a.targetOrderID:
			return a, legTarget, true
		}
	}
	return activeProtection{}, 0, false
}

// activeEpisodeIDs is the order protections are offered an observation in.
// It is a copy, because executing one protection can end another, and the
// thing being walked must not be the thing being changed.
func (p *protectionProjection) activeEpisodeIDs() []uint64 {
	out := make([]uint64, 0, len(p.active))
	for _, a := range p.active {
		out = append(out, a.episodeID)
	}
	return out
}

func absQty(q market.Qty) market.Qty {
	if q < 0 {
		return -q
	}
	return q
}

// entryGone notes that an order has been cancelled. If it carried a plan that
// never activated, the ending of that plan is now owed.
//
// A plan that has activated is not touched: the episode governs it, and the
// entry's remainder disappearing means nothing to what it already opened.
func (p *protectionProjection) entryGone(orderID string) {
	planned, ok := p.plannedFor(orderID)
	if !ok {
		return
	}
	p.owe(owedEnd(entryReference(orderID), planned.stopPrice, planned.targetPrice,
		ProtectionEntryCancelled, true))
}

func (p *protectionProjection) owe(events ...owedEvent) {
	p.owed = append(p.owed, events...)
}

// requireOwed refuses any event standing between a fact and what it requires,
// and reports whether this event was the thing owed.
//
// A caller needs to know: an ending that was owed has already had its levels
// checked here, against the protection as it stood when the fact that ended it
// happened — which is before the legs that fact cancels were taken off it.
func (p *protectionProjection) requireOwed(e Event) (bool, error) {
	if len(p.owed) == 0 {
		return false, nil
	}
	want := p.owed[0]
	// The first branch is for the diagnostic alone: an event of another kind
	// fails the checks below anyway, on a zero value, and says so far less
	// usefully. Naming what actually stood in the way is what a reader of a
	// rejected journal needs.
	if e.Header().Kind != want.kind {
		return false, fmt.Errorf("%w: %+v owes a %v and a %v followed instead",
			ErrProtectionOutlivedEntry, want.describe(), want.kind, e.Header().Kind)
	}

	if want.kind == KindOrderCancelled {
		cancelled, ok := e.(OrderCancelled)
		if !ok {
			return false, fmt.Errorf("%w: a %T is tagged as a cancellation", ErrProtectionOutlivedEntry, e)
		}
		if cancelled.OrderID != want.orderID {
			return false, fmt.Errorf("%w: %s owes a cancellation and %s was cancelled instead",
				ErrProtectionOutlivedEntry, want.orderID, cancelled.OrderID)
		}
		if cancelled.RemainingQty != want.remainingQty {
			return false, fmt.Errorf("%w: %s is cancelled with %d left, the events before it say %d",
				ErrProtectionOutlivedEntry, want.orderID, cancelled.RemainingQty, want.remainingQty)
		}
		if cancelled.Reason != want.cancelReason {
			return false, fmt.Errorf("%w: %s is cancelled for %v, the events before it say %v",
				ErrProtectionOutlivedEntry, want.orderID, cancelled.Reason, want.cancelReason)
		}
		p.owed = p.owed[1:]
		return true, nil
	}

	ended, ok := e.(ProtectionEnded)
	if !ok {
		return false, fmt.Errorf("%w: a %T is tagged as an ending", ErrProtectionOutlivedEntry, e)
	}
	if ended.Ref != want.ref {
		return false, fmt.Errorf("%w: the ending owed to %+v names %+v instead",
			ErrProtectionOutlivedEntry, want.ref, ended.Ref)
	}
	if want.checkReason && ended.Reason != want.endReason {
		return false, fmt.Errorf("%w: %+v ends with reason %v, the events before it say %v",
			ErrProtectionOutlivedEntry, want.ref, ended.Reason, want.endReason)
	}
	if ended.StopPrice != want.stopPrice || ended.TargetPrice != want.targetPrice {
		return false, fmt.Errorf("%w: %+v ends holding %d/%d, the events before it say %d/%d",
			ErrProtectionOutlivedEntry, want.ref, ended.StopPrice, ended.TargetPrice,
			want.stopPrice, want.targetPrice)
	}
	p.owed = p.owed[1:]
	return true, nil
}

// describe names what an owing is about, for a reader of a rejection.
func (o owedEvent) describe() string {
	if o.kind == KindOrderCancelled {
		return o.orderID
	}
	if o.ref.Kind == ProtectionRefEpisode {
		return fmt.Sprintf("episode %d", o.ref.EpisodeID)
	}
	return o.ref.OrderID
}

// settled reports a stream that stopped owing an ending it never wrote.
func (p *protectionProjection) settled() error {
	if len(p.owed) == 0 {
		return nil
	}
	return fmt.Errorf("%w: the log ends owing a %v about %s",
		ErrProtectionOutlivedEntry, p.owed[0].kind, p.owed[0].describe())
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

// ActiveProtection is a protection bound to an episode, as a reader outside the
// package sees it.
type ActiveProtection struct {
	EpisodeID     uint64
	StopPrice     market.Ticks
	TargetPrice   market.Ticks
	StopOrderID   string
	TargetOrderID string
	ProtectedQty  market.Qty
	Long          bool
}

func (p *protectionProjection) activeSnapshot() []ActiveProtection {
	out := make([]ActiveProtection, 0, len(p.active))
	for _, a := range p.active {
		out = append(out, ActiveProtection{
			EpisodeID: a.episodeID,
			StopPrice: a.stopPrice, TargetPrice: a.targetPrice,
			StopOrderID: a.stopOrderID, TargetOrderID: a.targetOrderID,
			ProtectedQty: a.protectedQty, Long: a.long,
		})
	}
	return out
}

func (p *protectionProjection) restore(planned []PlannedProtection, active []ActiveProtection, symbol string, used []string) {
	p.init()
	p.planned = p.planned[:0]
	for _, s := range planned {
		p.planned = append(p.planned, plannedProtection{
			entryOrderID: s.EntryOrderID,
			stopPrice:    s.StopPrice, targetPrice: s.TargetPrice,
			stopOrderID: s.StopOrderID, targetOrderID: s.TargetOrderID,
		})
	}
	p.active = p.active[:0]
	for _, a := range active {
		p.active = append(p.active, activeProtection{
			episodeID: a.EpisodeID, symbol: symbol,
			stopPrice: a.StopPrice, targetPrice: a.TargetPrice,
			stopOrderID: a.StopOrderID, targetOrderID: a.TargetOrderID,
			protectedQty: a.ProtectedQty, long: a.Long,
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

// validLevels refuses a protection that protects nothing, a level that is not a
// price, and levels on the wrong sides of each other.
//
// A long protected with the stop above the target has the two legs doing each
// other's job, and both can be reachable in a single observation — at which
// point which one executes is decided by the order the engine happens to visit
// them in, and the log would record an outcome the trader could not have
// predicted from what they placed. Equality is the same defect with no gap.
//
// It does not yet require the entry's price to lie between them. A market order
// can gap, and its fill is not known when the levels are decided; a level the
// market has already passed is a question for activation, which knows what the
// fill actually was. Deciding it here would mean rewriting the trader's
// decision with information they did not have.
func validLevels(long bool, stop, target market.Ticks) error {
	if stop == 0 && target == 0 {
		return ErrProtectionEmpty
	}
	if stop < 0 || target < 0 {
		return market.ErrNonPositivePrice
	}
	if stop == 0 || target == 0 {
		return nil
	}
	ordered := stop < target
	if !long {
		ordered = target < stop
	}
	if !ordered {
		return fmt.Errorf("%w: stop %d, target %d on a %s", ErrProtectionInverted, stop, target, sideWord(long))
	}
	return nil
}

func sideWord(long bool) string {
	if long {
		return "long"
	}
	return "short"
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
func (s *Session) SubmitOrderWithProtection(o market.Order, stop, target market.Ticks, decided Decision) error {
	return s.command(func() error { return s.submitOrderWithProtection(o, stop, target, decided) })
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
func (s *Session) submitOrderWithProtection(o market.Order, stop, target market.Ticks, decided Decision) error {
	// Every refusal happens before anything is recorded, so a rejected command
	// leaves no order, no protection, no reserved name, no counter moved and
	// no event.
	if err := validLevels(o.Side == market.SideBuy, stop, target); err != nil {
		return err
	}
	// The order's own faults are reported before the protection's. A duplicate
	// identifier is the more fundamental one: the protection exists only
	// because the order does, and naming the derived fault would send a reader
	// looking for a protection that was never the problem.
	prepared, err := s.prepareOrder(o, decided)
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
	// The protection meets the observation that activated it, not the next
	// one: an entry that filled through a gap may already be past its stop.
	if err := s.resolveProtection(prepared.at); err != nil {
		return err
	}
	return s.revalue(prepared.at)
}

// claimGesture records an act as taken, so a retry can be recognised as the
// same decision rather than becoming a second one.
func (s *Session) claimGesture(act Gesture) error { return s.gestures.claim(act) }

// Gestures is every human act this journal holds, and what each commanded. A
// caller answering a retry needs both: a set of spent names says the act
// happened and not what it did.
func (s *Session) Gestures() []Gesture { return s.gestures.snapshot() }

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
	s.gestures.attachProtection(stop, target)
	return s.protections.applyPlaced(placed)
}

// ReplaceProtection changes the levels of a protection that exists.
func (s *Session) ReplaceProtection(ref ProtectionRef, stop, target market.Ticks, decided Decision) error {
	return s.command(func() error { return s.replaceProtection(ref, stop, target, decided) })
}

func (s *Session) replaceProtection(ref ProtectionRef, stop, target market.Ticks, decided Decision) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := s.checkGesture(decided); err != nil {
		return err
	}
	current, at, err := s.protectionForCommand(ref)
	if err != nil {
		return err
	}
	// Replacing both levels with nothing is withdrawing the protection, and
	// must be said as that: CancelProtection names the decision it was.
	if err := validLevels(current.long, stop, target); err != nil {
		return err
	}

	var replaced ProtectionReplaced
	if err := s.record(at, KindProtectionReplaced, func(e Envelope) Event {
		stopID, targetID := current.stopOrderID, current.targetOrderID
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
			PreviousStopPrice: current.stopPrice, PreviousTargetPrice: current.targetPrice,
			StopPrice: stop, TargetPrice: target,
			StopOrderID: stopID, TargetOrderID: targetID,
			Widened: widened(current.long, current.stopPrice, stop),
			Decided: decided,
		}
		return replaced
	}); err != nil {
		return err
	}
	if err := s.claimGesture(Gesture{
		Kind: GestureReplaceProtection, Decided: decided, Ref: ref,
		StopPrice: stop, TargetPrice: target,
	}); err != nil {
		return err
	}
	return s.protections.applyReplaced(replaced)
}

// CancelProtection withdraws a protection, leaving its entry alone.
func (s *Session) CancelProtection(ref ProtectionRef, decided Decision) error {
	return s.command(func() error { return s.endProtection(ref, ProtectionWithdrawnByTrader, decided) })
}

func (s *Session) endProtection(ref ProtectionRef, reason ProtectionEndReason, decided Decision) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := s.checkGesture(decided); err != nil {
		return err
	}
	current, at, err := s.protectionForCommand(ref)
	if err != nil {
		return err
	}
	return s.recordProtectionEnded(at, current, ref, reason, decided)
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
	// Nobody decided this: the entry went and the plan could not survive it.
	return s.recordProtectionEnded(at, protectionLevels{
		stopPrice: planned.stopPrice, targetPrice: planned.targetPrice,
	}, ref, ProtectionEntryCancelled, Decision{})
}

// recordOwed writes what a fact required and folds it in.
func (s *Session) recordOwed(at market.LogicalTime, owed owedEvent) error {
	if owed.kind == KindOrderCancelled {
		if err := s.record(at, KindOrderCancelled, func(e Envelope) Event {
			return OrderCancelled{
				Envelope: e, OrderID: owed.orderID,
				RemainingQty: owed.remainingQty, Reason: owed.cancelReason,
			}
		}); err != nil {
			return err
		}
		s.protections.legCancelled(owed.orderID)
		return nil
	}
	return s.recordProtectionEnded(at, protectionLevels{
		stopPrice: owed.stopPrice, targetPrice: owed.targetPrice,
	}, owed.ref, owed.endReason, Decision{})
}

// recordProtectionEnded writes an ending and folds it in. Its caller has
// already resolved the protection and decided the command may proceed.
func (s *Session) recordProtectionEnded(at market.LogicalTime, levels protectionLevels, ref ProtectionRef, reason ProtectionEndReason, decided Decision) error {
	ended := ProtectionEnded{
		Ref: ref, StopPrice: levels.stopPrice, TargetPrice: levels.targetPrice,
		Reason: reason, Decided: decided,
	}
	if err := s.record(at, KindProtectionEnded, func(e Envelope) Event {
		ended.Envelope = e
		return ended
	}); err != nil {
		return err
	}
	if !decided.IsZero() {
		if err := s.claimGesture(Gesture{
			Kind: GestureWithdrawProtection, Decided: decided, Ref: ref,
		}); err != nil {
			return err
		}
	}
	return s.protections.applyEnded(ended)
}

// protectionLevels is the part of a protection every command acts on, whichever
// kind of reference names it and whichever state it is in.
type protectionLevels struct {
	stopPrice     market.Ticks
	targetPrice   market.Ticks
	stopOrderID   string
	targetOrderID string
	long          bool
}

// protectionForCommand resolves a reference and refuses everything a protection
// command cannot act on, before anything is recorded.
//
// A planned protection is named by its entry and an active one by its episode,
// and a command may not name the wrong one: once a plan activates the entry
// stops governing it, so a reference to that entry is stale and answering it
// would let a trader move levels through a name the system no longer uses.
func (s *Session) protectionForCommand(ref ProtectionRef) (protectionLevels, market.LogicalTime, error) {
	if !s.sessionOpen {
		return protectionLevels{}, 0, ErrNoSessionOpen
	}
	if s.ended() {
		return protectionLevels{}, 0, ErrChallengeEnded
	}
	if !s.hasQuote {
		return protectionLevels{}, 0, ErrNoMarketObserved
	}

	var levels protectionLevels
	if ref.Kind == ProtectionRefEpisode {
		active, ok := s.protections.activeFor(ref.EpisodeID)
		if !ok {
			return protectionLevels{}, 0, fmt.Errorf("%w: episode %d", ErrNoSuchProtection, ref.EpisodeID)
		}
		levels = protectionLevels{
			stopPrice: active.stopPrice, targetPrice: active.targetPrice,
			stopOrderID: active.stopOrderID, targetOrderID: active.targetOrderID,
			long: active.long,
		}
	} else {
		planned, ok := s.protections.plannedFor(ref.OrderID)
		if !ok {
			return protectionLevels{}, 0, fmt.Errorf("%w: %s", ErrNoSuchProtection, ref.OrderID)
		}
		long, err := s.entryOpensLong(planned.entryOrderID)
		if err != nil {
			return protectionLevels{}, 0, err
		}
		levels = protectionLevels{
			stopPrice: planned.stopPrice, targetPrice: planned.targetPrice,
			stopOrderID: planned.stopOrderID, targetOrderID: planned.targetOrderID,
			long: long,
		}
	}

	at := s.lastQuote.Time
	if err := s.journal.ValidateNext(at, s.sequence+1); err != nil {
		return protectionLevels{}, 0, err
	}
	return levels, at, nil
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

// ActiveProtections returns the protections bound to an open episode, in the
// order they were activated.
func (s *Session) ActiveProtections() []ActiveProtection { return s.protections.activeSnapshot() }

// legKind says which of a protection's two legs is being offered an
// observation.
type legKind uint8

const (
	legStop legKind = iota + 1
	legTarget
)

// protectiveOrder is the order a leg would be against this observation, and
// whether that level exists at all.
//
// It is built fresh from the protection's state each time rather than held as a
// working order, because the quantity a leg covers is the episode's exposure
// and that changes underneath it: a leg kept as an order would have to be
// rewritten on every fill, and the two copies would eventually disagree.
func protectiveOrder(a activeProtection, which legKind, instrument market.Instrument) (market.Order, bool) {
	// A protection closes the position, so it trades against it.
	side := market.SideSell
	if !a.long {
		side = market.SideBuy
	}
	switch which {
	case legStop:
		if a.stopPrice == 0 {
			return market.Order{}, false
		}
		o, err := market.NewStopOrder(a.stopOrderID, instrument, side, a.protectedQty, a.stopPrice)
		return o, err == nil
	default:
		if a.targetPrice == 0 {
			return market.Order{}, false
		}
		o, err := market.NewLimitOrder(a.targetOrderID, instrument, side, a.protectedQty, a.targetPrice)
		return o, err == nil
	}
}

// resolveProtection offers the observation to every active protection, the stop
// before the target.
//
// The order is fixed and it is the conservative one. Without a real queue
// position there is nothing in the data that says which of two reachable levels
// the market would have taken first, and a simulator that chose the target
// would be handing the trader the better of two outcomes it cannot know.
//
// It runs on the same observation that activated a protection, not the next
// one. An entry that filled through a gap may already be past its stop, and
// waiting would grant a survival the market never gave. The entry and the stop
// of a long trade against different sides of the book, so each keeps its own
// displayed size.
func (s *Session) resolveProtection(at market.LogicalTime) error {
	for _, id := range s.protections.activeEpisodeIDs() {
		for _, which := range []legKind{legStop, legTarget} {
			if err := s.executeLeg(at, id, which); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Session) executeLeg(at market.LogicalTime, episodeID uint64, which legKind) error {
	// The sibling may have closed the position a moment ago, and the episode
	// may have gone with it.
	active, ok := s.protections.activeFor(episodeID)
	if !ok {
		return nil
	}
	order, ok := protectiveOrder(active, which, s.cfg.Instrument)
	if !ok {
		return nil
	}

	result, err := s.policy.ExecuteOnQuote(order, s.lastQuote)
	if err != nil {
		return err
	}
	if len(result.Fills) == 0 && !result.StopTriggered {
		return nil
	}
	if err := s.consume(result.Fills); err != nil {
		return err
	}
	if _, err := s.applyAndRecord(at, result.Fills); err != nil {
		return err
	}

	// A stop that reached its level and found nothing at all has still
	// triggered, and a triggered stop cannot untrigger. There is no position
	// change to derive that from, so it is recorded here; a stop that filled in
	// part leaves a change behind, and the ending of what remains is derived
	// from it like everything else.
	if result.StopTriggered && len(result.Fills) == 0 {
		return s.recordOwed(at, owedCancel(order.ID, order.Qty, CancelledUnfillableRemainder))
	}
	return nil
}

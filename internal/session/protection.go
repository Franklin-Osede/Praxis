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

// owedEnding is a protection ending that the events so far require.
//
// The session records them; Replay and Verify recompute them and demand the
// journal holds exactly these, in this order, immediately. It is the same
// double entry the position changes already have: a derived fact is recomputed
// from what caused it rather than believed.
type owedEnding struct {
	ref         ProtectionRef
	stopPrice   market.Ticks
	targetPrice market.Ticks
	reason      ProtectionEndReason

	// checkReason says the owing knows why. Verify can see that a closing
	// episode's protection must end without seeing the fill, and the fill is
	// the only thing the reason turns on — a reversal ends it flipped, and
	// anything else ends it closed.
	checkReason bool
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

	// owed holds the endings the events so far require, in order. The very next
	// event must be the first of them: an ending and the fact that caused it
	// are one decision, so a reader that allowed anything between them would be
	// allowing a journal the producer cannot write.
	owed []owedEnding
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

// endingsAfter is what a position change requires to be ended, in the order the
// endings must be recorded. It changes nothing: an ending is an event, and only
// an event removes a protection.
//
// flip says this close is the first half of a reversal — the same fill opens
// the other side. It is knowable only from the whole effect of a fill, which is
// why the effect is assembled before any of it is written down. Deciding on the
// close alone would end the plan as having opened no exposure, a moment before
// the exposure it opens.
func (p *protectionProjection) endingsAfter(entryOrderID string, episodeID uint64, change portfolio.PositionEvent, flip bool) []owedEnding {
	var owed []owedEnding
	endPlan := func(reason ProtectionEndReason) {
		if planned, ok := p.plannedFor(entryOrderID); ok {
			owed = append(owed, owedEnding{
				ref:       ProtectionRef{Kind: ProtectionRefEntry, OrderID: entryOrderID},
				stopPrice: planned.stopPrice, targetPrice: planned.targetPrice,
				reason: reason, checkReason: true,
			})
		}
	}

	switch change.Kind {
	case portfolio.PositionClosed:
		// The episode is over, so whatever was protecting it is over with it.
		if active, ok := p.activeFor(episodeID); ok {
			reason := ProtectionPositionClosed
			if flip {
				reason = ProtectionFlipped
			}
			owed = append(owed, owedEnding{
				ref:       ProtectionRef{Kind: ProtectionRefEpisode, EpisodeID: episodeID},
				stopPrice: active.stopPrice, targetPrice: active.targetPrice,
				reason: reason, checkReason: true,
			})
		}
		// A plan on the closing order opened nothing — unless the same fill is
		// about to open the other side, which the plan then covers.
		if !flip {
			endPlan(ProtectionDidNotOpenExposure)
		}
	case portfolio.PositionReduced:
		endPlan(ProtectionDidNotOpenExposure)
	case portfolio.PositionIncreased:
		// A plan arriving at an episode that already has cover does not
		// silently replace it and is not kept beside it: it is ended, said out
		// loud, and the protection already there grows to the new exposure.
		if _, ok := p.activeFor(episodeID); ok {
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
	p.owe(owedEnding{
		ref:       ProtectionRef{Kind: ProtectionRefEntry, OrderID: orderID},
		stopPrice: planned.stopPrice, targetPrice: planned.targetPrice,
		reason: ProtectionEntryCancelled, checkReason: true,
	})
}

func (p *protectionProjection) owe(endings ...owedEnding) {
	p.owed = append(p.owed, endings...)
}

// requireEnding refuses any event standing between a fact and the ending it
// requires.
func (p *protectionProjection) requireEnding(e Event) error {
	if len(p.owed) == 0 {
		return nil
	}
	want := p.owed[0]
	// This first branch is for the diagnostic alone: an event of another kind
	// would fail the reference check below anyway, on a zero value, and say so
	// far less usefully. Naming what actually stood in the way is what a reader
	// of a rejected journal needs.
	ended, ok := e.(ProtectionEnded)
	if !ok {
		return fmt.Errorf("%w: %+v owes an ending and a %v followed instead",
			ErrProtectionOutlivedEntry, want.ref, e.Header().Kind)
	}
	if ended.Ref != want.ref {
		return fmt.Errorf("%w: the ending owed to %+v names %+v instead",
			ErrProtectionOutlivedEntry, want.ref, ended.Ref)
	}
	if want.checkReason && ended.Reason != want.reason {
		return fmt.Errorf("%w: %+v ends with reason %v, the events before it say %v",
			ErrProtectionOutlivedEntry, want.ref, ended.Reason, want.reason)
	}
	p.owed = p.owed[1:]
	return nil
}

// settled reports a stream that stopped owing an ending it never wrote.
func (p *protectionProjection) settled() error {
	if len(p.owed) == 0 {
		return nil
	}
	return fmt.Errorf("%w: the log ends owing the ending of %+v", ErrProtectionOutlivedEntry, p.owed[0].ref)
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
	if err := validLevels(o.Side == market.SideBuy, stop, target); err != nil {
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
	current, at, err := s.protectionForCommand(ref)
	if err != nil {
		return err
	}
	return s.recordProtectionEnded(at, current, ref, reason)
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
	return s.recordProtectionEnded(at, protectionLevels{
		stopPrice: planned.stopPrice, targetPrice: planned.targetPrice,
	}, ref, ProtectionEntryCancelled)
}

// recordProtectionEnded writes an ending and folds it in. Its caller has
// already resolved the protection and decided the command may proceed.
func (s *Session) recordProtectionEnded(at market.LogicalTime, levels protectionLevels, ref ProtectionRef, reason ProtectionEndReason) error {
	ended := ProtectionEnded{
		Ref: ref, StopPrice: levels.stopPrice, TargetPrice: levels.targetPrice,
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

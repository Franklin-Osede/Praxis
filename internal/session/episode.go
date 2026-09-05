package session

import (
	"fmt"

	"praxis/internal/market"
	"praxis/internal/portfolio"
)

// episodeProjection derives position episodes from the position changes a
// journal records.
//
// It is one implementation used in three places — the live session, Verify and
// Replay — because a second one would eventually disagree with the first, and
// the disagreement would be between a journal and the thing that checks it.
//
// It is pure in the sense that matters: it holds only what the events it has
// been given imply, reads no clock, and its answer depends on nothing but the
// order it saw them in.
//
// A trade is a position episode: the span during which the net position in one
// instrument stays non-zero and keeps its direction. See ADR-013.
type episodeProjection struct {
	// open holds only episodes that have not ended. It is a slice rather than
	// a map because the order episodes are classified in decides the streak,
	// and that must never depend on iteration order.
	open []openEpisode

	// consecutiveLosingTrades counts completed episodes that ended at a loss,
	// in an unbroken run. An open episode is not counted: it is not a loser
	// until it ends, and treating it as one would report a streak the trader
	// has not had.
	consecutiveLosingTrades uint32
}

type openEpisode struct {
	symbol string

	// id is the journal sequence of the PositionChanged that opened it, which
	// is deterministic and needs no registry.
	id uint64

	netQty      market.Qty
	realisedCts market.Cents
	feesCts     market.Cents
}

// apply folds one position change into the projection. The sequence is the
// journal position of the event, which becomes the identity of an episode this
// change opens.
func (p *episodeProjection) apply(sequence uint64, change portfolio.PositionEvent) error {
	symbol := change.Instrument.Symbol
	at := p.indexOf(symbol)

	if at < 0 {
		if change.Kind != portfolio.PositionOpened {
			return fmt.Errorf("%w: a %v in %s with no episode open", ErrEpisode, change.Kind, symbol)
		}
		p.open = append(p.open, openEpisode{symbol: symbol, id: sequence})
		at = len(p.open) - 1
	}

	episode := &p.open[at]
	var err error
	if episode.feesCts, err = market.AddCents(episode.feesCts, change.FeeCts); err != nil {
		return err
	}
	if episode.netQty, err = market.AddQty(episode.netQty, signedQty(change.Side, change.Qty)); err != nil {
		return err
	}

	switch change.Kind {
	case portfolio.PositionOpened, portfolio.PositionIncreased:
		return nil
	case portfolio.PositionReduced:
		episode.realisedCts, err = market.AddCents(episode.realisedCts, change.RealisedCts)
		return err
	case portfolio.PositionClosed:
		if episode.realisedCts, err = market.AddCents(episode.realisedCts, change.RealisedCts); err != nil {
			return err
		}
		return p.close(at)
	default:
		return fmt.Errorf("%w: unknown change %v", ErrEpisode, change.Kind)
	}
}

// close classifies an episode that has reached flat and removes it.
//
// The result is realised P&L less every leg's commission, and break-even
// includes fees: an episode that gave back its gain in commission was not
// economically flat. See ADR-013.
func (p *episodeProjection) close(at int) error {
	episode := p.open[at]
	result, err := market.SubCents(episode.realisedCts, episode.feesCts)
	if err != nil {
		return err
	}
	if result < 0 {
		if p.consecutiveLosingTrades, err = addOrders(p.consecutiveLosingTrades, 1); err != nil {
			return err
		}
	} else {
		p.consecutiveLosingTrades = 0
	}
	p.open = append(p.open[:at], p.open[at+1:]...)
	return nil
}

func (p *episodeProjection) indexOf(symbol string) int {
	for n, e := range p.open {
		if e.symbol == symbol {
			return n
		}
	}
	return -1
}

// foldPositionChange folds one position change into both projections and
// reports what the change requires to be recorded next.
//
// It is one implementation with three callers — the live session, Replay and
// Verify — for the reason every shared projection here exists: the glue is
// eight lines of ordering that must be identical in all three, and the last
// time it was copied it drifted in the one dimension the copies could not
// agree on. `record` is how each caller discharges an obligation: the session
// writes it, a reader queues it to demand of the journal next.
//
// flip says this close is the first half of a reversal, and knowFlip whether
// the caller can tell. See consequencesOf.
func foldPositionChange(
	episodes *episodeProjection, protections *protectionProjection,
	sequence uint64, fillOrderID string, change portfolio.PositionEvent,
	flip, knowFlip bool, record func(owedEvent) error,
) error {
	symbol := change.Instrument.Symbol
	// An episode's identity is the sequence of the change that opened it, so a
	// change that opens one must be read after the fold and every other before.
	episodeID, _ := episodes.episodeID(symbol)
	if err := episodes.apply(sequence, change); err != nil {
		return err
	}
	if change.Kind == portfolio.PositionOpened {
		episodeID, _ = episodes.episodeID(symbol)
	}

	net := episodes.netQtyOf(symbol)
	for _, owed := range protections.consequencesOf(fillOrderID, episodeID, change, net, flip, knowFlip) {
		if err := record(owed); err != nil {
			return err
		}
	}
	protections.bind(fillOrderID, episodeID, change, net)
	return nil
}

// consecutiveLosingTradesNow is the streak as it stands.
//
// A decision taken before an episode ends carries the streak that was true when
// it was taken, which is what the trader knew. The order that flips a position
// therefore carries the count from before the flip, and the next order sees the
// episode the flip closed.
func (p *episodeProjection) consecutiveLosingTradesNow() uint32 {
	return p.consecutiveLosingTrades
}

// netQtyOf is the signed net quantity of the open episode in an instrument.
// A closed episode leaves nothing, which is zero exposure and not an absence.
func (p *episodeProjection) netQtyOf(symbol string) market.Qty {
	if at := p.indexOf(symbol); at >= 0 {
		return p.open[at].netQty
	}
	return 0
}

// episodeID is the identity of the open episode in an instrument, and whether
// there is one. Protection placed before any fill has no episode to attach to,
// which is why it is bound to the entry's order instead.
func (p *episodeProjection) episodeID(symbol string) (uint64, bool) {
	if at := p.indexOf(symbol); at >= 0 {
		return p.open[at].id, true
	}
	return 0, false
}

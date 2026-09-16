package session

import (
	"errors"
	"fmt"
	"strings"

	"praxis/internal/challenge"
	"praxis/internal/execution"
	"praxis/internal/market"
	"praxis/internal/portfolio"
)

// Errors reported when a command cannot describe something the session can do.
var (
	ErrSessionAlreadyOpen = errors.New("session: a trading session is already open")
	ErrNoSessionOpen      = errors.New("session: no trading session is open")
	ErrChallengeEnded     = errors.New("session: the evaluation has ended")
	ErrNoMarketObserved   = errors.New("session: no market observation yet")
	ErrWrongInstrument    = errors.New("session: not the session's instrument")

	// ErrInconsistentConfig guards two starting balances that must agree. The
	// account and the evaluation are built from different fields, and a
	// mismatch would only surface when the first session refused to open,
	// after an invalid configuration had already been recorded.
	ErrInconsistentConfig = errors.New("session: account and evaluation disagree about the starting balance")

	// ErrPacingWithoutSubject reports a configuration that says how the
	// observations reached a person and does not say which person, or names a
	// person and says nobody was there. The two are one claim: a pilot or
	// confirmatory session is by definition somebody's, and a scripted run is
	// by definition nobody's.
	ErrPacingWithoutSubject = errors.New("session: the pacing and the subject disagree about whether a person was there")

	// ErrRunWithoutIdentity reports a journal somebody traded that does not say
	// which execution it is. An anchor can certify no such journal, and that has
	// to be refused where it is written rather than discovered at collection.
	ErrRunWithoutIdentity = errors.New("session: this run was traded and does not name itself")

	// ErrSessionNeedsRecovery is terminal. A commit that failed leaves the
	// outcome unknown — the batch may be whole on disk, or partial, or absent
	// — and only reading the journal can say which. The session therefore
	// stops rather than guessing, and is replaced by one rebuilt from the
	// confirmed batches. It is never healed in place and its last command is
	// never re-executed automatically.
	ErrSessionNeedsRecovery = errors.New("session: requires recovery")

	// ErrOverdrawnBook reports a fill that took more depth than its
	// observation displayed. Execution cannot produce one; only a journal can.
	ErrOverdrawnBook = errors.New("session: a fill took more than the book showed")

	ErrDuplicateOrderID = errors.New("session: an order with this identifier is already working")
	ErrNoSuchOrder      = errors.New("session: no working order with this identifier")
)

// workingOrder is an order waiting for a later observation.
type workingOrder struct {
	order     market.Order
	remaining market.Qty
}

// BatchCommitter durably records the events one command produced.
//
// Commit means the whole batch is durably confirmed. Any error means the
// outcome is unknown until recovery examines the store, so the caller must not
// retry it. Batch numbers, checksums, paths and durability policy belong to
// the store: a session knows only that one command produced one ordered group
// of events.
type BatchCommitter interface {
	Commit(events []Event) error
}

// Session is one deterministic run: an account, an evaluation, an execution
// policy and the journal of everything they did.
type Session struct {
	cfg     Config
	account *portfolio.Account
	eval    *challenge.Challenge
	policy  execution.ConservativeExecution
	journal *Journal

	// sequence is the session's one ordering. Every journal event takes the
	// next value, and the challenge engine is fed the sequence of the event
	// that caused the input, so both streams share one order.
	sequence uint64

	lastQuote market.Quote
	hasQuote  bool

	// clock is the chronology of everything a person did to this journal. It
	// is the same machine Replay and Verify run, so the writing side can never
	// commit a stamp its own readers refuse.
	clock interactionClock

	// lastObserved is the journal position of the observation now on the
	// screen, and presented the one an interface has confirmed showing. A
	// human command is refused until they agree, because an interval from a
	// presentation nobody confirmed has no beginning.
	lastObserved uint64
	presented    PresentationID

	// working holds the orders waiting for a later observation, in the order
	// they were submitted. It is a slice and not a map because the order in
	// which they are offered an observation decides which of them fills a
	// scarce book first, and that must never depend on iteration order.
	working []workingOrder

	openSessionID       challenge.SessionID
	sessionOpen         bool
	observedThisSession bool

	ordersThisSession  uint32
	consecutiveLosses  uint32
	sessionRealisedCts market.Cents

	// protections derives planned protections and the identifiers a journal
	// has spent. The same projection runs in Verify and in Replay.
	protections protectionProjection

	// gestures is every human act this journal holds and what each of them
	// commanded. It answers a retry: the same act with the same command is
	// already committed, and with a different one is a conflict.
	gestures gestureIndex

	// episodes derives position episodes from the changes it is given. The
	// same projection runs in Verify and in Replay, so a live session and the
	// checks on its journal cannot disagree about what a trade was.
	episodes episodeProjection

	// committer is optional. A session without one keeps its journal in
	// memory and nothing else.
	committer BatchCommitter

	// needsRecovery holds the failure that made this session unusable. It
	// never clears.
	needsRecovery error
}

// New starts a session and records its configuration. A nil committer keeps
// the journal in memory only.
func New(cfg Config, at market.LogicalTime, committer BatchCommitter) (*Session, error) {
	if err := cfg.Instrument.Validate(); err != nil {
		return nil, err
	}
	// A subject nobody can write down would poison the very first batch.
	// Empty is legitimate — it says nobody traded this — so it is the one
	// identifier allowed to be absent.
	if cfg.SubjectID != "" {
		if err := market.ValidIdentifier(cfg.SubjectID); err != nil {
			return nil, err
		}
	}
	if err := pacingAgreesWithSubject(cfg); err != nil {
		return nil, err
	}
	if err := pacingAgreesWithRunIdentity(cfg); err != nil {
		return nil, err
	}
	if cfg.StartingBalanceCts != cfg.Rules.StartingBalanceCts {
		return nil, fmt.Errorf("%w: account %d, evaluation %d",
			ErrInconsistentConfig, cfg.StartingBalanceCts, cfg.Rules.StartingBalanceCts)
	}
	account, err := portfolio.NewAccount(cfg.StartingBalanceCts, cfg.CommissionPerContractCts)
	if err != nil {
		return nil, err
	}
	eval, err := challenge.New(cfg.Rules)
	if err != nil {
		return nil, err
	}

	s := &Session{
		cfg: cfg, account: account, eval: eval, journal: &Journal{}, committer: committer,
		clock: interactionClock{subjectID: cfg.SubjectID, pacing: cfg.Pacing},
	}
	if err := s.command(func() error {
		return s.record(at, KindSessionStarted, func(e Envelope) Event {
			return SessionStarted{Envelope: e, Config: cfg}
		})
	}); err != nil {
		return nil, err
	}
	return s, nil
}

// pacingAgreesWithRunIdentity refuses a journal a person traded that cannot say
// which execution it is.
//
// A scripted run may carry one or not: it is an execution like any other, and
// nothing certifies it. A pilot or confirmatory one must, because an anchor
// names the run it certifies and there would be nothing to name — and because
// the moment to find that out is when the journal is written, not when an
// experimenter is standing over it at collection.
func pacingAgreesWithRunIdentity(cfg Config) error {
	if cfg.RunID != "" {
		return market.ValidIdentifier(cfg.RunID)
	}
	if cfg.Pacing == PacingScripted {
		return nil
	}
	return fmt.Errorf("%w: %v pacing and no run identity", ErrRunWithoutIdentity, cfg.Pacing)
}

// pacingAgreesWithSubject refuses a configuration that is two claims at once.
//
// A pilot or confirmatory session is somebody's by definition, and a scripted
// run is nobody's. Allowing a scripted journal to name a subject would let a
// fixture be labelled as though a person had traded it, which is the one thing
// the pilot sample must never contain.
func pacingAgreesWithSubject(cfg Config) error {
	traded := cfg.Pacing != PacingScripted
	named := cfg.SubjectID != ""
	if traded == named {
		return nil
	}
	if traded {
		return fmt.Errorf("%w: %v pacing and nobody named", ErrPacingWithoutSubject, cfg.Pacing)
	}
	return fmt.Errorf("%w: %s named and nothing was presented to them",
		ErrPacingWithoutSubject, cfg.SubjectID)
}

// NeedsRecovery reports the failure that made this session unusable, or nil.
func (s *Session) NeedsRecovery() error { return s.needsRecovery }

// command runs one public command as a single boundary: the events it produced
// are committed together, or the session stops.
//
// The aggregates mutate before the commit is attempted. That is the accepted
// shape from ADR-012 — a copy of the state would be a second representation
// whose only consumer is the transaction, and whose bugs would be silent — and
// it is why a failed commit is terminal rather than something to retry.
func (s *Session) command(run func() error) error {
	if s.needsRecovery != nil {
		return fmt.Errorf("%w: %w", ErrSessionNeedsRecovery, s.needsRecovery)
	}

	start := s.journal.Len()
	err := run()
	produced := s.journal.EventsSince(start)

	if err != nil {
		// A command that refused before recording anything leaves the session
		// usable. One that failed after recording has already put events in
		// the journal that will never be committed, so the next command's
		// batch would start from a position the store has never seen — which
		// the writer would refuse, correctly and much later.
		//
		// Working orders made this reachable. An observation records itself
		// before offering the quote to a waiting stop, and an order records
		// itself before its fills are applied, so a refusal from the account
		// now happens after the log has already spoken. It is covered by
		// TestACommandThatFailsAfterRecordingStopsTheSession.
		if len(produced) > 0 {
			s.needsRecovery = err
			return fmt.Errorf("%w: %w", ErrSessionNeedsRecovery, err)
		}
		return err
	}
	if s.committer == nil || len(produced) == 0 {
		return nil
	}
	if err := s.committer.Commit(produced); err != nil {
		s.needsRecovery = err
		return fmt.Errorf("%w: %w", ErrSessionNeedsRecovery, err)
	}
	return nil
}

// record appends one event at the next position in the session's single
// ordering. The sequence advances only once the append has succeeded, so a
// rejected command leaves no gap and the numbering stays contiguous.
func (s *Session) record(at market.LogicalTime, k Kind, build func(Envelope) Event) error {
	env := Envelope{Time: at, Sequence: s.sequence + 1, Kind: k}
	e := build(env)
	// Every event, not only the head of a command. The head has already been
	// checked at the door, where a refusal costs nothing; this is the derived
	// ones, where a stamp is a programming fault rather than bad input, and
	// where the write path and the read path must be running the same rule or
	// the asymmetry this machine exists to close reopens one event at a time.
	if err := s.clock.Check(e); err != nil {
		return err
	}
	if err := s.journal.Append(e); err != nil {
		return err
	}
	s.sequence++
	s.clock.Apply(e)
	return nil
}

func (s *Session) ended() bool {
	state := s.eval.State()
	return state == challenge.StateFailed || state == challenge.StatePassed
}

func (s *Session) marks() ([]portfolio.Mark, error) {
	p, _ := s.account.Position(s.cfg.Instrument)
	return marksFor(s.cfg.Instrument, p, s.lastQuote, s.hasQuote)
}

// marksFor values an open position at the price it could actually be closed
// at: a long at the bid, a short at the ask. A midpoint or the entry side
// would show money the position could not realise.
//
// It is a free function because replay must value an account the same way a
// live session did. Two implementations of this rule would let a journal
// record a valuation no session could have produced.
func marksFor(i market.Instrument, p portfolio.Position, q market.Quote, hasQuote bool) ([]portfolio.Mark, error) {
	if p.IsFlat() {
		return nil, nil
	}
	if !hasQuote {
		return nil, ErrNoMarketObserved
	}
	price := q.Bid
	if p.IsShort() {
		price = q.Ask
	}
	return []portfolio.Mark{{Instrument: i, Price: price}}, nil
}

// valueAccount is the one way an account becomes a pair of figures, used by a
// live session and by replay alike.
func valueAccount(a *portfolio.Account, i market.Instrument, q market.Quote, hasQuote bool) (balanceCts, equityCts market.Cents, err error) {
	if balanceCts, err = a.BalanceCts(); err != nil {
		return 0, 0, err
	}
	p, _ := a.Position(i)
	marks, err := marksFor(i, p, q, hasQuote)
	if err != nil {
		return 0, 0, err
	}
	if equityCts, err = a.EquityCts(marks); err != nil {
		return 0, 0, err
	}
	return balanceCts, equityCts, nil
}

// value takes both figures from one account valuation at one set of marks, so
// a rule reading balance and a rule reading equity cannot disagree about when
// they were measured.
func (s *Session) value() (balanceCts, equityCts market.Cents, err error) {
	return valueAccount(s.account, s.cfg.Instrument, s.lastQuote, s.hasQuote)
}

// revalue values the account and feeds the evaluation. It is called after
// every observation and after every order, so an unrealised loss can end an
// evaluation without a trade being closed.
//
// The ended() half of its guard is currently unreachable: every caller refuses
// a terminal evaluation before it gets here. It stays because this is the only
// path that feeds the evaluation and it has four callers, so its precondition
// is stated here rather than assumed of all of them — the same reason
// EndTradingSession's missing gate is written down rather than left to be
// inferred. If it ever fires, a caller acquired a terminal state mid-command,
// and that is a fact worth not stepping over.
func (s *Session) revalue(at market.LogicalTime) error {
	if !s.sessionOpen || s.ended() {
		return nil
	}
	balance, equity, err := s.value()
	if err != nil {
		return err
	}
	decisions, err := s.eval.Observe(challenge.AccountSnapshot{
		Time: at, Sequence: s.sequence + 1, SessionID: s.openSessionID,
		BalanceCts: balance, EquityCts: equity,
	})
	if err != nil {
		return err
	}
	if err := s.record(at, KindAccountValued, func(e Envelope) Event {
		return AccountValued{Envelope: e, SessionID: s.openSessionID, BalanceCts: balance, EquityCts: equity}
	}); err != nil {
		return err
	}
	return s.appendDecisions(at, decisions)
}

func (s *Session) appendDecisions(at market.LogicalTime, decisions []challenge.Event) error {
	for _, d := range decisions {
		if err := s.record(at, KindChallengeDecision, func(e Envelope) Event {
			return ChallengeDecision{Envelope: e, CausedBySequence: d.Sequence, Decision: d.Decision}
		}); err != nil {
			return err
		}
	}
	return nil
}

// Events returns a copy of the journal. The journal itself is not exposed:
// an outside Append would advance its ordering without advancing the session's
// sequence, and the two would silently disagree from then on.
func (s *Session) Events() []Event { return s.journal.Events() }

// JournalLen is the number of events recorded so far.
func (s *Session) JournalLen() int { return s.journal.Len() }

func (s *Session) Account() *portfolio.Account     { return s.account }
func (s *Session) Challenge() *challenge.Challenge { return s.eval }

// Valuation is an account's two figures, taken together at one set of marks.
// A rule reading balance and a rule reading equity must never disagree about
// when they were measured, and a caller handed them separately eventually gets
// one of each.
type Valuation struct {
	BalanceCts market.Cents
	EquityCts  market.Cents
}

// Valuation values the account at the book the session last saw, by the one
// implementation of that rule.
//
// It is exported because an adapter that showed the participant a different
// figure would falsify what OrderContext claims they knew — and the only way to
// be sure the screen and the journal agree is that neither computes it.
// Deriving it outside was a second implementation of the marking rule, and it
// answered balance on every error, so a participant holding a losing position
// could be shown money they did not have.
func (s *Session) Valuation() (Valuation, error) {
	balance, equity, err := s.value()
	if err != nil {
		return Valuation{}, err
	}
	return Valuation{BalanceCts: balance, EquityCts: equity}, nil
}

// ConsecutiveLosingTrades is the streak of completed losing episodes as it
// stands. It is what OrderContext will record on the next decision, so an
// interface that shows the participant anything else makes that field's
// documented meaning false.
func (s *Session) ConsecutiveLosingTrades() uint32 {
	return s.episodes.consecutiveLosingTradesNow()
}

// LastObserved is the journal position of the observation on the screen, and
// zero when none is. It is what names a presentation, so an interface has to be
// able to send it back: a confirmation that could not say which observation it
// confirmed would let a stale tab stand in for the one in front of somebody.
func (s *Session) LastObserved() uint64 { return s.lastObserved }

// LastQuote is the observation the session is standing on, and whether it has
// seen one. It is the book as the session holds it, which is the book as it was
// left: the displayed size is consumed with every fill, so what this returns is
// what is still there rather than what the file showed.
//
// An adapter that read the file instead would put depth on the screen that the
// participant's own order had already taken, and the next order sized against
// it would be sized against a quantity that is not there.
func (s *Session) LastQuote() (market.Quote, bool) { return s.lastQuote, s.hasQuote }

// Gesture is what one human act commanded, and whether the journal holds it.
//
// It answers one identifier rather than handing back every act: the loop is
// asked to recognise a retry on every request that carries a gesture, and
// walking a copy of the whole register to do it is the same mistake a command
// used to make with the whole journal.
func (s *Session) Gesture(id string) (Gesture, bool) { return s.gestures.find(id) }

// ChallengeEnded reports whether the evaluation has reached a terminal state.
// An adapter driving a file asks it to stop consuming, and stopping is not an
// error: the evaluation ending is the result.
func (s *Session) ChallengeEnded() bool { return s.ended() }

// TradingSessionOpen reports whether a trading session is open.
//
// It exists because openness was being inferred from a non-empty identifier in
// three separate places, and the identifier outlived the session it named. A
// boolean derived from a name is a second representation of a fact the session
// already holds, and the two drifted the moment a session ended: the name
// stayed, so a reader asking it carried on into the next boundary as though one
// were still running.
func (s *Session) TradingSessionOpen() bool { return s.sessionOpen }

// OpenSessionID is the identifier of the open trading session, and empty when
// none is open. It is never the name of one that has closed.
func (s *Session) OpenSessionID() challenge.SessionID { return s.openSessionID }

// OpenTradingSession asserts a session boundary and immediately values the
// account into it.
func (s *Session) OpenTradingSession(at market.LogicalTime, id challenge.SessionID) error {
	return s.command(func() error { return s.openTradingSession(at, id) })
}

func (s *Session) openTradingSession(at market.LogicalTime, id challenge.SessionID) error {
	if s.sessionOpen {
		return ErrSessionAlreadyOpen
	}
	// A boundary the journal cannot write down is a boundary the evaluation
	// must not accept: it would move the challenge into a session the log
	// then refuses, with nothing to reconcile them.
	if err := market.ValidIdentifier(string(id)); err != nil {
		return err
	}
	if s.ended() {
		return ErrChallengeEnded
	}
	// Nothing may decide before the journal has agreed to record the result.
	if err := s.journal.ValidateNext(at, s.sequence+1); err != nil {
		return err
	}
	balance, equity, err := s.value()
	if err != nil {
		return err
	}

	// The evaluation decides before anything is written: it can reject the
	// boundary, and a rejected command must leave no trace in the journal.
	decisions, err := s.eval.OpenSession(challenge.SessionOpened{
		Time: at, Sequence: s.sequence + 1, SessionID: id,
		BalanceCts: balance, EquityCts: equity,
	})
	if err != nil {
		return err
	}
	if err := s.record(at, KindSessionOpened, func(e Envelope) Event {
		return SessionOpened{Envelope: e, SessionID: id, BalanceCts: balance, EquityCts: equity}
	}); err != nil {
		return err
	}
	if err := s.appendDecisions(at, decisions); err != nil {
		return err
	}

	s.openSessionID, s.sessionOpen, s.observedThisSession = id, true, false
	s.ordersThisSession, s.consecutiveLosses, s.sessionRealisedCts = 0, 0, 0
	return s.revalue(at)
}

// Observe accepts a market observation and revalues the account against it.
// Observe accepts a market observation and revalues the account against it.
// sourceSequence is the position the source gave it; with the observation's
// logical time it is what orders the stream.
func (s *Session) Observe(q market.Quote, sourceSequence uint64) error {
	return s.command(func() error { return s.observe(q, sourceSequence) })
}

// offerObservation gives an observation to everything waiting for one: the
// protections already standing, then every working order in the order it was
// submitted, with each order's own protection resolved before the next order
// is offered anything.
//
// The interleaving is the point. A working order can fill and activate a
// protection whose stop this same observation has already passed; leaving every
// protection to the end would let the next working order take the liquidity
// that stop should have found. Priority to the protection is the conservative
// reading, and it is the one the whole engine already takes.
//
// It runs before the account is revalued, so the valuation the evaluation sees
// already contains anything a stop just did. Revaluing first would report an
// equity that had not yet felt the fill the same observation caused.
func (s *Session) offerObservation(at market.LogicalTime) error {
	if err := s.resolveProtection(at); err != nil {
		return err
	}
	// A fresh slice, not s.working[:0]: reusing the backing array would
	// overwrite the entry being read while the loop is still walking it.
	kept := make([]workingOrder, 0, len(s.working))
	for _, w := range s.working {
		attempt := w.order
		attempt.Qty = w.remaining

		result, err := s.policy.ExecuteOnQuote(attempt, s.lastQuote)
		if err != nil {
			return err
		}
		if err := s.consume(result.Fills); err != nil {
			return err
		}
		filled, err := s.applyAndRecord(at, result.Fills)
		if err != nil {
			return err
		}
		remaining, err := market.AddQty(w.remaining, -filled)
		if err != nil {
			return err
		}
		switch {
		case remaining <= 0:
		// A stop that reached its level has become a market order, and a
		// market order does not wait. Leaving it working would let it
		// untrigger when the price came back, and the trader would be
		// protected by an instruction the market had already passed.
		case result.StopTriggered:
			if err := s.cancelRemainder(at, w.order.ID, remaining); err != nil {
				return err
			}
		default:
			w.remaining = remaining
			kept = append(kept, w)
		}
		// Whatever this order just did to the position, its protection meets
		// the same observation before the next order sees any of it.
		if err := s.resolveProtection(at); err != nil {
			return err
		}
	}
	s.working = kept
	return nil
}

// consume takes what filled out of the observation's displayed size.
func (s *Session) consume(fills []market.Fill) error {
	consumed, err := consumeBook(s.lastQuote, fills)
	if err != nil {
		return err
	}
	s.lastQuote = consumed
	return nil
}

// consumeBook takes what filled out of an observation's displayed size.
//
// One observation shows a finite book. Several waiting orders, a protective
// leg and a newly submitted order all draw from the same displayed size, and
// once it is gone nothing more fills until the next observation. Letting each
// of them take the full size would hand the same contracts to everybody, which
// is the largest possible way for a simulator to invent liquidity.
//
// Replay consumes it too, from the fills the journal records. A reconstruction
// that started each observation from the untouched quote would resume with
// depth the interrupted run had already spent, and would let Replay believe a
// cancellation the book at that moment could not have produced.
func consumeBook(q market.Quote, fills []market.Fill) (market.Quote, error) {
	for _, f := range fills {
		var err error
		if f.Side == market.SideBuy {
			q.AskSize, err = market.AddQty(q.AskSize, -f.Qty)
		} else {
			q.BidSize, err = market.AddQty(q.BidSize, -f.Qty)
		}
		if err != nil {
			return market.Quote{}, err
		}
		// A book cannot go negative. In a live session it cannot: execution
		// stops at the size the quote displayed. A journal is not so
		// constrained, and a negative book is the arithmetic saying a fill
		// took depth the observation never showed.
		if q.BidSize < 0 || q.AskSize < 0 {
			return market.Quote{}, fmt.Errorf("%w: %s took %d from a book showing %d/%d",
				ErrOverdrawnBook, f.OrderID, f.Qty, q.BidSize, q.AskSize)
		}
	}
	return q, nil
}

// fillEffect is everything one fill did, held together.
//
// Protection is decided from the whole of it and not from each change in turn.
// A reversal produces a close and an open from a single fill, and a decision
// taken on the close alone would end the plan as having opened no exposure — a
// moment before the exposure it opens. See ADR-014.
type fillEffect struct {
	fill    market.Fill
	changes []portfolio.PositionEvent
}

// applyAndRecord applies fills to the account and records what they did,
// returning the quantity filled. Nothing is recorded until every fill has been
// accepted, so a refusal leaves no trace.
func (s *Session) applyAndRecord(at market.LogicalTime, fills []market.Fill) (market.Qty, error) {
	effects := make([]fillEffect, 0, len(fills))
	var filled market.Qty
	for _, f := range fills {
		changes, err := s.account.ApplyFill(f)
		if err != nil {
			return 0, err
		}
		effects = append(effects, fillEffect{fill: f, changes: changes})
		total, err := market.AddQty(filled, f.Qty)
		if err != nil {
			return 0, err
		}
		filled = total
	}

	for _, effect := range effects {
		fill := effect.fill
		if err := s.record(at, KindFillProduced, func(e Envelope) Event {
			return FillProduced{Envelope: e, Fill: fill}
		}); err != nil {
			return 0, err
		}
		for n, change := range effect.changes {
			if err := s.record(at, KindPositionChanged, func(e Envelope) Event {
				return PositionChanged{Envelope: e, Change: change}
			}); err != nil {
				return 0, err
			}
			if err := s.countClose(change); err != nil {
				return 0, err
			}
			// A close with more of the same fill still to come is a reversal.
			// The projections are fed the sequence the change was recorded at,
			// which becomes the identity of any episode it opens.
			flip := change.Kind == portfolio.PositionClosed && n+1 < len(effect.changes)
			if err := foldPositionChange(&s.episodes, &s.protections, s.sequence, fill.OrderID,
				change, flip, true, func(owed owedEvent) error {
					return s.recordOwed(at, owed)
				}); err != nil {
				return 0, err
			}
		}
	}
	return filled, nil
}

func (s *Session) observe(q market.Quote, sourceSequence uint64) error {
	// The journal ends where the evaluation ends. Market after that point is
	// market nobody can act on, and the file it came from still holds it, so
	// recording it would be a second copy — one that is not free, because an
	// observation belongs to a trading session and keeping it means crossing
	// the next boundary onto an evaluation that is over.
	if s.ended() {
		return ErrChallengeEnded
	}
	// An observation belongs to a trading session, and is refused before
	// anything is written when none is open. Waiting orders and protections are
	// offered every observation before the account is revalued, and with no
	// session open nothing can be offered it — so accepting it would move the
	// last book past a price no stop had seen, and the next open would value
	// the account against that price inside the batch that opens the session.
	// Replay holds journals to the same rule.
	if !s.sessionOpen {
		return fmt.Errorf("%w: an observation at %d has no session to belong to", ErrNoSessionOpen, q.Time)
	}
	if q.Instrument != s.cfg.Instrument {
		return ErrWrongInstrument
	}
	if err := q.Validate(); err != nil {
		return err
	}
	if err := s.journal.ValidateNext(q.Time, s.sequence+1); err != nil {
		return err
	}
	if err := s.record(q.Time, KindMarketObserved, func(e Envelope) Event {
		return MarketObserved{Envelope: e, Quote: q, SourceSequence: sourceSequence}
	}); err != nil {
		return err
	}
	s.lastQuote, s.hasQuote, s.observedThisSession = q, true, true
	s.lastObserved = s.sequence

	if err := s.offerObservation(q.Time); err != nil {
		return err
	}
	return s.revalue(q.Time)
}

// SubmitOrder records a decision, executes it, applies its fills and revalues.
//
// decided is when the person acted, by their own clock. The kernel records it
// and never reads it: it arrives as data from whatever witnessed the act, the
// same shape as an observation's source sequence.
func (s *Session) SubmitOrder(o market.Order, decided Decision) error {
	return s.command(func() error { return s.submitOrder(o, decided) })
}

// preparedOrder is everything a submission has decided before any of it is
// recorded: the context it was taken in, what the book would give it, and what
// would be left over.
//
// Preparation and recording are separate because an order may carry a
// protection, and the protection has to be written between the decision and
// its first fill. Reusing the whole of submitOrder cannot do that, and writing
// the entry a second time inside the protected path would be two producers of
// one event, which is exactly how two journals come to disagree.
type preparedOrder struct {
	order     market.Order
	context   OrderContext
	at        market.LogicalTime
	decided   Decision
	result    execution.Result
	remaining market.Qty
}

func (s *Session) submitOrder(o market.Order, decided Decision) error {
	prepared, err := s.prepareOrder(o, decided)
	if err != nil {
		return err
	}
	if err := s.recordOrder(prepared); err != nil {
		return err
	}
	if err := s.executeOrder(prepared); err != nil {
		return err
	}
	if err := s.resolveProtection(prepared.at); err != nil {
		return err
	}
	return s.revalue(prepared.at)
}

// prepareOrder refuses everything a submission cannot do and computes
// everything it will do. It changes nothing: no order in the log, no counter
// moved, no money touched, no name spent.
func (s *Session) prepareOrder(o market.Order, decided Decision) (preparedOrder, error) {
	if !s.sessionOpen {
		return preparedOrder{}, ErrNoSessionOpen
	}
	if s.ended() {
		return preparedOrder{}, ErrChallengeEnded
	}
	if o.Instrument != s.cfg.Instrument {
		return preparedOrder{}, ErrWrongInstrument
	}
	if err := o.Validate(); err != nil {
		return preparedOrder{}, err
	}
	// An order executes against an observation of this trading session. The
	// last book of a previous session is stale, and using it would also place
	// the fill before the boundary in logical time.
	if !s.observedThisSession {
		return preparedOrder{}, ErrNoMarketObserved
	}
	if strings.HasPrefix(o.ID, reservedPrefix) {
		return preparedOrder{}, fmt.Errorf("%w: %s", ErrReservedNamespace, o.ID)
	}
	if s.isWorking(o.ID) {
		return preparedOrder{}, fmt.Errorf("%w: %s", ErrDuplicateOrderID, o.ID)
	}
	if s.protections.used(o.ID) {
		return preparedOrder{}, fmt.Errorf("%w: %s", ErrOrderIDReused, o.ID)
	}
	if err := s.checkDecision(KindOrderSubmitted, decided); err != nil {
		return preparedOrder{}, err
	}
	if err := s.requirePresented(decided); err != nil {
		return preparedOrder{}, err
	}
	at := s.lastQuote.Time
	if err := s.journal.ValidateNext(at, s.sequence+1); err != nil {
		return preparedOrder{}, err
	}

	position, _ := s.account.Position(s.cfg.Instrument)
	balance, equity, err := s.value()
	if err != nil {
		return preparedOrder{}, err
	}
	context := OrderContext{
		BalanceCts:                 balance,
		EquityCts:                  equity,
		OrdersSubmittedThisSession: s.ordersThisSession,
		ConsecutiveLosses:          s.consecutiveLosses,
		SessionRealisedCts:         s.sessionRealisedCts,
		PositionQtyBefore:          position.NetQty,
		ConsecutiveLosingTrades:    s.episodes.consecutiveLosingTradesNow(),
	}

	result, err := s.policy.ExecuteOnQuote(o, s.lastQuote)
	if err != nil {
		return preparedOrder{}, err
	}
	var filled market.Qty
	for _, f := range result.Fills {
		if filled, err = market.AddQty(filled, f.Qty); err != nil {
			return preparedOrder{}, err
		}
	}
	remaining, err := market.AddQty(o.Qty, -filled)
	if err != nil {
		return preparedOrder{}, err
	}
	return preparedOrder{
		order: o, context: context, at: at, decided: decided,
		result: result, remaining: remaining,
	}, nil
}

// recordOrder writes the decision and moves what the decision alone moves.
func (s *Session) recordOrder(p preparedOrder) error {
	if err := s.record(p.at, KindOrderSubmitted, func(e Envelope) Event {
		return OrderSubmitted{Envelope: e, Order: p.order, Context: p.context, Decided: p.decided}
	}); err != nil {
		return err
	}
	if err := s.protections.claim(p.order.ID); err != nil {
		return err
	}
	if err := s.claimGesture(Gesture{
		Kind: GestureSubmitOrder, Decided: p.decided, Order: p.order,
	}); err != nil {
		return err
	}
	var err error
	s.ordersThisSession, err = addOrders(s.ordersThisSession, 1)
	return err
}

// executeOrder takes what filled out of the book, applies it, and records what
// became of the rest.
func (s *Session) executeOrder(p preparedOrder) error {
	if err := s.consume(p.result.Fills); err != nil {
		return err
	}
	if _, err := s.applyAndRecord(p.at, p.result.Fills); err != nil {
		return err
	}
	if p.remaining <= 0 {
		return nil
	}
	// What the book could not fill is a fact either way, and the log says
	// which. A limit or a stop waits for a later observation; a market order's
	// remainder is cancelled, because resting it would mean inventing a price
	// the trader never named.
	if p.order.Type == market.OrderTypeMarket || p.result.StopTriggered {
		return s.cancelRemainder(p.at, p.order.ID, p.remaining)
	}
	if err := s.record(p.at, KindOrderRested, func(e Envelope) Event {
		return OrderRested{Envelope: e, Order: p.order, RestingQty: p.remaining}
	}); err != nil {
		return err
	}
	s.working = append(s.working, workingOrder{order: p.order, remaining: p.remaining})
	return nil
}

// cancelRemainder records the part of an order the book could not fill and
// that will not wait for another observation.
func (s *Session) cancelRemainder(at market.LogicalTime, orderID string, remaining market.Qty) error {
	if err := s.record(at, KindOrderCancelled, func(e Envelope) Event {
		return OrderCancelled{
			Envelope: e, OrderID: orderID, RemainingQty: remaining,
			Reason: CancelledUnfillableRemainder,
		}
	}); err != nil {
		return err
	}
	return s.endPlanFor(at, orderID)
}

// countClose updates the behavioural counters from a closing leg. Only a leg
// that realised money is a win or a loss; opening or adding is neither.
func (s *Session) countClose(change portfolio.PositionEvent) error {
	if change.Kind != portfolio.PositionReduced && change.Kind != portfolio.PositionClosed {
		return nil
	}
	total, err := market.AddCents(s.sessionRealisedCts, change.RealisedCts)
	if err != nil {
		return err
	}
	s.sessionRealisedCts = total
	if change.RealisedCts < 0 {
		if s.consecutiveLosses, err = addOrders(s.consecutiveLosses, 1); err != nil {
			return err
		}
	} else {
		s.consecutiveLosses = 0
	}
	return nil
}

// WorkingOrders returns the orders waiting for a later observation, in the
// order they were submitted.
func (s *Session) WorkingOrders() []market.Order {
	out := make([]market.Order, 0, len(s.working))
	for _, w := range s.working {
		resting := w.order
		resting.Qty = w.remaining
		out = append(out, resting)
	}
	return out
}

func (s *Session) isWorking(id string) bool {
	for _, w := range s.working {
		if w.order.ID == id {
			return true
		}
	}
	return false
}

// CancelOrder withdraws a working order.
func (s *Session) CancelOrder(id string, decided Decision) error {
	return s.command(func() error { return s.cancelOrder(id, decided) })
}

func (s *Session) cancelOrder(id string, decided Decision) error {
	if !s.sessionOpen {
		return ErrNoSessionOpen
	}
	if err := s.checkDecision(KindOrderCancelled, decided); err != nil {
		return err
	}
	// A cancellation is a decision like any other, and its interval runs from
	// the same beginning. This was the one human command that did not ask.
	if err := s.requirePresented(decided); err != nil {
		return err
	}
	if s.ended() {
		return ErrChallengeEnded
	}
	at := s.lastQuote.Time
	if !s.hasQuote {
		return ErrNoMarketObserved
	}
	if err := s.journal.ValidateNext(at, s.sequence+1); err != nil {
		return err
	}

	for n, w := range s.working {
		if w.order.ID != id {
			continue
		}
		if err := s.record(at, KindOrderCancelled, func(e Envelope) Event {
			return OrderCancelled{
				Envelope: e, OrderID: id, RemainingQty: w.remaining,
				Reason: CancelledByTrader, Decided: decided,
			}
		}); err != nil {
			return err
		}
		if err := s.claimGesture(Gesture{
			Kind: GestureCancelOrder, Decided: decided, OrderID: id,
		}); err != nil {
			return err
		}
		s.working = append(s.working[:n], s.working[n+1:]...)
		return s.endPlanFor(at, id)
	}
	return fmt.Errorf("%w: %s", ErrNoSuchOrder, id)
}

// EndTradingSession closes the open trading session.
func (s *Session) EndTradingSession(at market.LogicalTime) error {
	return s.command(func() error { return s.endTradingSession(at) })
}

// endTradingSession has no ended() gate and OpenTradingSession does. The
// asymmetry reads like an oversight and is not: opening a session on an
// evaluation that is over would ask the challenge engine to leave a terminal
// state, and closing one asks nothing of it. Closing the books after an
// evaluation ends is a legitimate operator act. It was only ever a defect when
// it happened automatically inside a run that then could not continue, and the
// terminal check at the top of Drive removes that run.
func (s *Session) endTradingSession(at market.LogicalTime) error {
	if !s.sessionOpen {
		return ErrNoSessionOpen
	}
	id := s.openSessionID
	if err := s.record(at, KindSessionEnded, func(e Envelope) Event {
		return SessionEnded{Envelope: e, SessionID: id}
	}); err != nil {
		return err
	}
	// The name goes with the session. Keeping it would leave the only fact a
	// reader could ask pointing at something that is over.
	s.openSessionID, s.sessionOpen, s.observedThisSession = "", false, false
	return nil
}

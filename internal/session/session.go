package session

import (
	"errors"
	"fmt"

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

	// ErrSessionNeedsRecovery is terminal. A commit that failed leaves the
	// outcome unknown — the batch may be whole on disk, or partial, or absent
	// — and only reading the journal can say which. The session therefore
	// stops rather than guessing, and is replaced by one rebuilt from the
	// confirmed batches. It is never healed in place and its last command is
	// never re-executed automatically.
	ErrSessionNeedsRecovery = errors.New("session: requires recovery")
)

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

	openSessionID       challenge.SessionID
	sessionOpen         bool
	observedThisSession bool

	ordersThisSession  uint32
	consecutiveLosses  uint32
	sessionRealisedCts market.Cents

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

	s := &Session{cfg: cfg, account: account, eval: eval, journal: &Journal{}, committer: committer}
	if err := s.command(func() error {
		return s.record(at, KindSessionStarted, func(e Envelope) Event {
			return SessionStarted{Envelope: e, Config: cfg}
		})
	}); err != nil {
		return nil, err
	}
	return s, nil
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
		return fmt.Errorf("%w: %v", ErrSessionNeedsRecovery, s.needsRecovery)
	}

	start := s.journal.Len()
	err := run()
	produced := s.journal.Events()[start:]

	if err != nil {
		// A command that refused before recording anything leaves the session
		// usable. One that failed after recording has already moved memory
		// past the disk.
		//
		// NOT COVERED BY A TEST. No public command can currently fail after
		// recording an event: every one of them either refuses before touching
		// the journal or runs to completion. The guard is here because that is
		// a property of today's commands rather than of the design. If a
		// change ever opens that path, its test belongs in the same commit.
		if len(produced) > 0 {
			s.needsRecovery = err
			return fmt.Errorf("%w: %v", ErrSessionNeedsRecovery, err)
		}
		return err
	}
	if s.committer == nil || len(produced) == 0 {
		return nil
	}
	if err := s.committer.Commit(produced); err != nil {
		s.needsRecovery = err
		return fmt.Errorf("%w: %v", ErrSessionNeedsRecovery, err)
	}
	return nil
}

// record appends one event at the next position in the session's single
// ordering. The sequence advances only once the append has succeeded, so a
// rejected command leaves no gap and the numbering stays contiguous.
func (s *Session) record(at market.LogicalTime, k Kind, build func(Envelope) Event) error {
	env := Envelope{Time: at, Sequence: s.sequence + 1, Kind: k}
	if err := s.journal.Append(build(env)); err != nil {
		return err
	}
	s.sequence++
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

func (s *Session) Account() *portfolio.Account        { return s.account }
func (s *Session) Challenge() *challenge.Challenge    { return s.eval }
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
func (s *Session) Observe(q market.Quote) error {
	return s.command(func() error { return s.observe(q) })
}

func (s *Session) observe(q market.Quote) error {
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
		return MarketObserved{Envelope: e, Quote: q}
	}); err != nil {
		return err
	}
	s.lastQuote, s.hasQuote, s.observedThisSession = q, true, true
	return s.revalue(q.Time)
}

// SubmitOrder records a decision, executes it, applies its fills and revalues.
func (s *Session) SubmitOrder(o market.Order) error {
	return s.command(func() error { return s.submitOrder(o) })
}

func (s *Session) submitOrder(o market.Order) error {
	if !s.sessionOpen {
		return ErrNoSessionOpen
	}
	if s.ended() {
		return ErrChallengeEnded
	}
	if o.Instrument != s.cfg.Instrument {
		return ErrWrongInstrument
	}
	if err := o.Validate(); err != nil {
		return err
	}
	// An order executes against an observation of this trading session. The
	// last book of a previous session is stale, and using it would also place
	// the fill before the boundary in logical time.
	if !s.observedThisSession {
		return ErrNoMarketObserved
	}
	at := s.lastQuote.Time
	if err := s.journal.ValidateNext(at, s.sequence+1); err != nil {
		return err
	}

	position, _ := s.account.Position(s.cfg.Instrument)
	balance, equity, err := s.value()
	if err != nil {
		return err
	}
	context := OrderContext{
		BalanceCts:                 balance,
		EquityCts:                  equity,
		OrdersSubmittedThisSession: s.ordersThisSession,
		ConsecutiveLosses:          s.consecutiveLosses,
		SessionRealisedCts:         s.sessionRealisedCts,
		PositionQtyBefore:          position.NetQty,
	}

	// Everything the command will do is computed and applied before any of it
	// is recorded, so a refusal leaves no order in the log, no counter moved
	// and no money changed. Execution against one quote yields at most one
	// fill today; when a single order can produce several, this needs a real
	// transaction boundary rather than the ordering of these lines.
	fills, err := s.policy.ExecuteOnQuote(o, s.lastQuote)
	if err != nil {
		return err
	}
	applied := make([][]portfolio.PositionEvent, 0, len(fills))
	for _, f := range fills {
		changes, err := s.account.ApplyFill(f)
		if err != nil {
			return err
		}
		applied = append(applied, changes)
	}

	if err := s.record(at, KindOrderSubmitted, func(e Envelope) Event {
		return OrderSubmitted{Envelope: e, Order: o, Context: context}
	}); err != nil {
		return err
	}
	if s.ordersThisSession, err = addOrders(s.ordersThisSession, 1); err != nil {
		return err
	}

	for n, f := range fills {
		if err := s.record(at, KindFillProduced, func(e Envelope) Event {
			return FillProduced{Envelope: e, Fill: f}
		}); err != nil {
			return err
		}
		for _, change := range applied[n] {
			if err := s.record(at, KindPositionChanged, func(e Envelope) Event {
				return PositionChanged{Envelope: e, Change: change}
			}); err != nil {
				return err
			}
			if err := s.countClose(change); err != nil {
				return err
			}
		}
	}
	return s.revalue(at)
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

// EndTradingSession closes the open trading session.
func (s *Session) EndTradingSession(at market.LogicalTime) error {
	return s.command(func() error { return s.endTradingSession(at) })
}

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
	s.sessionOpen, s.observedThisSession = false, false
	return nil
}

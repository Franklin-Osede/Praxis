package session

import (
	"errors"

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
)

// Session is one deterministic run: an account, an evaluation, an execution
// policy and the journal of everything they did.
type Session struct {
	cfg     Config
	account *portfolio.Account
	eval    *challenge.Challenge
	policy  execution.ConservativeExecution
	journal Journal

	// sequence is the session's one ordering. Every journal event takes the
	// next value, and the challenge engine is fed the sequence of the event
	// that caused the input, so both streams share one order.
	sequence uint64

	lastQuote market.Quote
	hasQuote  bool

	openSessionID       challenge.SessionID
	sessionOpen         bool
	observedThisSession bool

	tradesThisSession  uint32
	consecutiveLosses  uint32
	sessionRealisedCts market.Cents
}

// New starts a session and records its configuration.
func New(cfg Config, at market.LogicalTime) (*Session, error) {
	if err := cfg.Instrument.Validate(); err != nil {
		return nil, err
	}
	account, err := portfolio.NewAccount(cfg.StartingBalanceCts, cfg.CommissionPerContractCts)
	if err != nil {
		return nil, err
	}
	eval, err := challenge.New(cfg.Rules)
	if err != nil {
		return nil, err
	}

	s := &Session{cfg: cfg, account: account, eval: eval}
	if err := s.record(at, KindSessionStarted, func(e Envelope) Event {
		return SessionStarted{Envelope: e, Config: cfg}
	}); err != nil {
		return nil, err
	}
	return s, nil
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

// marks values an open position at the price it could actually be closed at:
// a long at the bid, a short at the ask. A midpoint or the entry side would
// show money the position could not realise.
func (s *Session) marks() ([]portfolio.Mark, error) {
	p, ok := s.account.Position(s.cfg.Instrument)
	if !ok || p.IsFlat() {
		return nil, nil
	}
	if !s.hasQuote {
		return nil, ErrNoMarketObserved
	}
	price := s.lastQuote.Bid
	if p.IsShort() {
		price = s.lastQuote.Ask
	}
	return []portfolio.Mark{{Instrument: s.cfg.Instrument, Price: price}}, nil
}

// value takes both figures from one account valuation at one set of marks, so
// a rule reading balance and a rule reading equity cannot disagree about when
// they were measured.
func (s *Session) value() (balanceCts, equityCts market.Cents, err error) {
	if balanceCts, err = s.account.BalanceCts(); err != nil {
		return 0, 0, err
	}
	marks, err := s.marks()
	if err != nil {
		return 0, 0, err
	}
	if equityCts, err = s.account.EquityCts(marks); err != nil {
		return 0, 0, err
	}
	return balanceCts, equityCts, nil
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
			return ChallengeDecision{Envelope: e, Decision: d}
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) Journal() *Journal                  { return &s.journal }
func (s *Session) Account() *portfolio.Account        { return s.account }
func (s *Session) Challenge() *challenge.Challenge    { return s.eval }
func (s *Session) OpenSessionID() challenge.SessionID { return s.openSessionID }

// OpenTradingSession asserts a session boundary and immediately values the
// account into it.
func (s *Session) OpenTradingSession(at market.LogicalTime, id challenge.SessionID) error {
	if s.sessionOpen {
		return ErrSessionAlreadyOpen
	}
	if s.ended() {
		return ErrChallengeEnded
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
	s.tradesThisSession, s.consecutiveLosses, s.sessionRealisedCts = 0, 0, 0
	return s.revalue(at)
}

// Observe accepts a market observation and revalues the account against it.
func (s *Session) Observe(q market.Quote) error {
	if q.Instrument != s.cfg.Instrument {
		return ErrWrongInstrument
	}
	if err := q.Validate(); err != nil {
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

	position, _ := s.account.Position(s.cfg.Instrument)
	balance, equity, err := s.value()
	if err != nil {
		return err
	}
	context := OrderContext{
		BalanceCts:         balance,
		EquityCts:          equity,
		TradesThisSession:  s.tradesThisSession,
		ConsecutiveLosses:  s.consecutiveLosses,
		SessionRealisedCts: s.sessionRealisedCts,
		PositionQtyBefore:  position.NetQty,
	}

	at := s.lastQuote.Time
	if err := s.record(at, KindOrderSubmitted, func(e Envelope) Event {
		return OrderSubmitted{Envelope: e, Order: o, Context: context}
	}); err != nil {
		return err
	}
	s.tradesThisSession++

	fills, err := s.policy.ExecuteOnQuote(o, s.lastQuote)
	if err != nil {
		return err
	}
	for _, f := range fills {
		if err := s.record(at, KindFillProduced, func(e Envelope) Event {
			return FillProduced{Envelope: e, Fill: f}
		}); err != nil {
			return err
		}
		changes, err := s.account.ApplyFill(f)
		if err != nil {
			return err
		}
		for _, change := range changes {
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
		s.consecutiveLosses++
	} else {
		s.consecutiveLosses = 0
	}
	return nil
}

// EndTradingSession closes the open trading session.
func (s *Session) EndTradingSession(at market.LogicalTime) error {
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

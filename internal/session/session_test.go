package session_test

import (
	"errors"
	"testing"

	"praxis/internal/challenge"
	"praxis/internal/market"
	"praxis/internal/portfolio"
	"praxis/internal/session"
)

var mnq = market.Instrument{Symbol: "MNQ", CentsPerTick: 50}

func config() session.Config {
	return session.Config{
		Instrument: mnq,
		// Somebody traded this, which is what makes the human clock the
		// helpers stamp coherent. A journal carrying one and no subject is
		// refused, and so is the reverse.
		SubjectID:                "t-01",
		Pacing:                   session.PacingPilot,
		StartingBalanceCts:       5_000_000,
		CommissionPerContractCts: 50,
		Rules: challenge.Rules{
			StartingBalanceCts: 5_000_000,
			MaxDailyLossCts:    100_000,
			ProfitTargetCts:    100_000,
			MaxTotalLossCts:    200_000,
		},
	}
}

func newSession(t *testing.T) *session.Session {
	t.Helper()
	gestures = 0
	s, err := session.New(config(), 1_000, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func quote(at market.LogicalTime, bid, ask market.Ticks) market.Quote {
	return market.Quote{
		Instrument: mnq, Time: at,
		Bid: bid, Ask: ask, BidSize: 100, AskSize: 100,
	}
}

func order(id string, side market.Side, qty market.Qty) market.Order {
	o, err := market.NewMarketOrder(id, mnq, side, qty)
	if err != nil {
		panic(err)
	}
	return o
}

func kinds(events []session.Event) []session.Kind {
	var out []session.Kind
	for _, e := range events {
		out = append(out, e.Header().Kind)
	}
	return out
}

func mustOpen(t *testing.T, s *session.Session, at market.LogicalTime, id challenge.SessionID) {
	t.Helper()
	if err := s.OpenTradingSession(at, id); err != nil {
		t.Fatalf("OpenTradingSession: %v", err)
	}
}

func mustObserve(t *testing.T, s *session.Session, q market.Quote) {
	t.Helper()
	if err := s.Observe(q, 1); err != nil {
		t.Fatalf("Observe: %v", err)
	}
}

// gestures numbers the acts a test performs, so each carries its own identifier
// the way a real client's counter would. newSession resets it, which is what
// keeps a scripted run reproducible.
var gestures int

// decided is a stand-in for a person acting: a gesture, a moment in the world,
// and a monotonic reading inside one run of uninterrupted interaction. Tests
// that care about the interval between decisions build their own.
func decided(elapsed session.ElapsedNanos) session.Decision {
	gestures++
	return session.Decision{
		GestureID:    "g-" + itoa(uint64(gestures)),
		AtUTCNanos:   session.UnixNanos(1_764_000_000_000_000_000 + int64(elapsed)),
		Segment:      1,
		ElapsedNanos: elapsed,
	}
}

// decidedAt is a fresh gesture at the start of the segment, which is what most
// tests want: an act distinct from every other act, with no interval to speak of.
func decidedAt() session.Decision { return decided(0) }

func mustSubmit(t *testing.T, s *session.Session, o market.Order) {
	t.Helper()
	if err := s.SubmitOrder(o, decidedAt()); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
}

// Scenario: a session records its configuration before anything else
func TestNewRecordsTheConfiguration(t *testing.T) {
	s := newSession(t)

	events := s.Events()
	if len(events) != 1 {
		t.Fatalf("events: got %d, want only the start", len(events))
	}
	started, ok := events[0].(session.SessionStarted)
	if !ok {
		t.Fatalf("first event: got %T, want SessionStarted", events[0])
	}
	if started.Config != config() {
		t.Fatalf("config: got %+v, want %+v", started.Config, config())
	}
	if started.Sequence != 1 {
		t.Fatalf("sequence: got %d, want 1", started.Sequence)
	}
}

// Scenario: opening a trading session values the account into it immediately
//
//	Given a started session
//	When a trading session opens
//	Then the boundary, the challenge decisions it produced, and an account
//	  valuation all follow, in that order, so the evaluation is never left
//	  holding a reference it has not measured anything against.
func TestOpeningATradingSessionValuesTheAccountImmediately(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")

	want := []session.Kind{
		session.KindSessionStarted,
		session.KindSessionOpened,
		session.KindChallengeDecision, // activated
		session.KindChallengeDecision, // session reference established
		session.KindAccountValued,
	}
	got := kinds(s.Events())
	if len(got) != len(want) {
		t.Fatalf("kinds: got %v, want %v", got, want)
	}
	for n := range want {
		if got[n] != want[n] {
			t.Fatalf("kinds: got %v, want %v", got, want)
		}
	}
	if s.Challenge().State() != challenge.StateActive {
		t.Fatalf("challenge: got %v, want active", s.Challenge().State())
	}
}

// Scenario: no order is accepted before a session opens
func TestAnOrderBeforeASessionOpensIsRejected(t *testing.T) {
	s := newSession(t)
	if err := s.SubmitOrder(order("o-1", market.SideBuy, 1), decidedAt()); !errors.Is(err, ErrNoSessionOpenSentinel) {
		t.Fatalf("error: got %v, want %v", err, ErrNoSessionOpenSentinel)
	}
	if s.JournalLen() != 1 {
		t.Fatalf("a rejected order was recorded: %v", kinds(s.Events()))
	}
}

var ErrNoSessionOpenSentinel = session.ErrNoSessionOpen

// Scenario: an order needs a book from this trading session to execute against
//
// The last book of a previous session is stale, and using it would also place
// the fill before the boundary in logical time.
func TestAnOrderBeforeAnyObservationIsRejected(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")

	if err := s.SubmitOrder(order("o-1", market.SideBuy, 1), decidedAt()); !errors.Is(err, session.ErrNoMarketObserved) {
		t.Fatalf("error: got %v, want %v", err, session.ErrNoMarketObserved)
	}
}

// Scenario: an order becomes fills, position changes and a fresh valuation
func TestSubmittingAnOrderProducesTheWholeChain(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, quote(3_000, 20_000, 20_001))
	mustSubmit(t, s, order("o-1", market.SideBuy, 2))

	got := kinds(s.Events())
	tail := got[len(got)-4:]
	want := []session.Kind{
		session.KindOrderSubmitted,
		session.KindFillProduced,
		session.KindPositionChanged,
		session.KindAccountValued,
	}
	for n := range want {
		if tail[n] != want[n] {
			t.Fatalf("chain: got %v, want it to end %v", got, want)
		}
	}

	p, ok := s.Account().Position(mnq)
	if !ok || p.NetQty != 2 || p.CostBasisCts != 2_000_100 {
		t.Fatalf("position: got %+v, want long 2 at the ask", p)
	}
}

// Scenario: a position is marked at the price it could actually be closed at
//
//	Given a long position
//	When the account is valued
//	Then the mark is the bid, not the ask and not a midpoint, because the
//	  bid is what the position would fetch. A short is marked at the ask.
func TestPositionsAreMarkedAtTheExitSide(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, quote(3_000, 20_000, 20_001))
	mustSubmit(t, s, order("o-1", market.SideBuy, 2))

	// Bought 2 at the ask 20001, so cost is 2,000,100 cents. Marked at the
	// bid of 20000 the position is worth 2,000,000: a spread's worth of loss
	// before anything has moved.
	valued := lastValuation(t, s)
	wantEquity := market.Cents(5_000_000) - 100 /* commission */ - 100 /* spread */
	if valued.EquityCts != wantEquity {
		t.Fatalf("equity: got %d, want %d", valued.EquityCts, wantEquity)
	}
	if valued.BalanceCts != 5_000_000-100 {
		t.Fatalf("balance: got %d, want the starting balance less commission", valued.BalanceCts)
	}

	// The bid moved up ten ticks: two contracts at 50 cents a tick is 1,000
	// cents better than the previous mark, leaving 900 of unrealised gain
	// over a cost taken at the ask.
	mustObserve(t, s, quote(4_000, 20_010, 20_011))
	valued = lastValuation(t, s)
	if valued.EquityCts != 5_000_000-100+900 {
		t.Fatalf("equity: got %d, want 5000800", valued.EquityCts)
	}
}

func lastValuation(t *testing.T, s *session.Session) session.AccountValued {
	t.Helper()
	events := s.Events()
	for n := len(events) - 1; n >= 0; n-- {
		if v, ok := events[n].(session.AccountValued); ok {
			return v
		}
	}
	t.Fatal("no valuation was recorded")
	return session.AccountValued{}
}

// Scenario: a decision records what the account looked like when it was taken
func TestOrderSubmittedCarriesItsContext(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, quote(3_000, 20_000, 20_001))

	mustSubmit(t, s, order("o-1", market.SideBuy, 2))
	mustObserve(t, s, quote(4_000, 19_900, 19_901))
	mustSubmit(t, s, order("o-2", market.SideSell, 2)) // closes at a loss
	mustSubmit(t, s, order("o-3", market.SideBuy, 1))

	third := orderContext(t, s, "o-3")
	if third.OrdersSubmittedThisSession != 2 {
		t.Fatalf("trades: got %d, want 2 before this one", third.OrdersSubmittedThisSession)
	}
	if third.ConsecutiveLosses != 1 {
		t.Fatalf("consecutive losses: got %d, want 1", third.ConsecutiveLosses)
	}
	if third.SessionRealisedCts >= 0 {
		t.Fatalf("session realised: got %d, want a loss", third.SessionRealisedCts)
	}
	if third.PositionQtyBefore != 0 {
		t.Fatalf("position before: got %d, want flat", third.PositionQtyBefore)
	}

	second := orderContext(t, s, "o-2")
	if second.PositionQtyBefore != 2 {
		t.Fatalf("position before: got %d, want long 2", second.PositionQtyBefore)
	}
	if second.ConsecutiveLosses != 0 {
		t.Fatalf("consecutive losses: got %d, want none yet", second.ConsecutiveLosses)
	}
}

func orderContext(t *testing.T, s *session.Session, orderID string) session.OrderContext {
	t.Helper()
	for _, e := range s.Events() {
		if o, ok := e.(session.OrderSubmitted); ok && o.Order.ID == orderID {
			return o.Context
		}
	}
	t.Fatalf("no OrderSubmitted for %s", orderID)
	return session.OrderContext{}
}

// Scenario: an ended evaluation blocks trading
func TestATerminalChallengeBlocksFurtherOrders(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, quote(3_000, 20_000, 20_001))
	mustSubmit(t, s, order("o-1", market.SideBuy, 20))

	// A fall well past the daily limit, with the position still open.
	mustObserve(t, s, quote(4_000, 19_800, 19_801))
	if s.Challenge().State() != challenge.StateFailed {
		t.Fatalf("challenge: got %v, want failed", s.Challenge().State())
	}

	if err := s.SubmitOrder(order("o-2", market.SideSell, 1), decidedAt()); !errors.Is(err, session.ErrChallengeEnded) {
		t.Fatalf("error: got %v, want %v", err, session.ErrChallengeEnded)
	}

	before := s.JournalLen()
	mustObserve(t, s, quote(5_000, 19_700, 19_701))
	if s.JournalLen() != before+1 {
		t.Fatalf("an observation after the end produced more than the observation itself")
	}
}

// Scenario: boundaries are explicit in both directions
func TestTradingSessionsOpenAndCloseExplicitly(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")

	if err := s.OpenTradingSession(3_000, "d2"); !errors.Is(err, session.ErrSessionAlreadyOpen) {
		t.Fatalf("error: got %v, want %v", err, session.ErrSessionAlreadyOpen)
	}

	if err := s.EndTradingSession(4_000); err != nil {
		t.Fatalf("EndTradingSession: %v", err)
	}
	if err := s.EndTradingSession(5_000); !errors.Is(err, session.ErrNoSessionOpen) {
		t.Fatalf("error: got %v, want %v", err, session.ErrNoSessionOpen)
	}

	mustOpen(t, s, 6_000, "d2")
	if s.OpenSessionID() != "d2" {
		t.Fatalf("session: got %v, want d2", s.OpenSessionID())
	}
}

// Scenario: counters are per trading session
func TestTheSessionCountersResetAtABoundary(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")
	mustObserve(t, s, quote(3_000, 20_000, 20_001))
	mustSubmit(t, s, order("o-1", market.SideBuy, 1))
	if err := s.EndTradingSession(4_000); err != nil {
		t.Fatalf("EndTradingSession: %v", err)
	}

	mustOpen(t, s, 5_000, "d2")
	mustObserve(t, s, quote(6_000, 20_000, 20_001))
	mustSubmit(t, s, order("o-2", market.SideSell, 1))

	ctx := orderContext(t, s, "o-2")
	if ctx.OrdersSubmittedThisSession != 0 {
		t.Fatalf("trades: got %d, want the counter reset", ctx.OrdersSubmittedThisSession)
	}
	if ctx.SessionRealisedCts != 0 {
		t.Fatalf("session realised: got %d, want the counter reset", ctx.SessionRealisedCts)
	}
	if ctx.PositionQtyBefore != 1 {
		t.Fatalf("position before: got %d, want the position to survive the boundary", ctx.PositionQtyBefore)
	}
}

// An observation for another instrument is not this session's business.
func TestRejectsAnObservationForAnotherInstrument(t *testing.T) {
	s := newSession(t)
	mustOpen(t, s, 2_000, "d1")

	other := quote(3_000, 20_000, 20_001)
	other.Instrument = market.Instrument{Symbol: "MES", CentsPerTick: 125}
	if err := s.Observe(other, 1); !errors.Is(err, session.ErrWrongInstrument) {
		t.Fatalf("error: got %v, want %v", err, session.ErrWrongInstrument)
	}
}

var _ = portfolio.Position{}

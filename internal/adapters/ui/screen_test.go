package ui_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"praxis/internal/adapters/marketdata"
	"praxis/internal/adapters/persistence"
	"praxis/internal/market"
	"praxis/internal/session"
)

// twoLosersThenAnOrder walks a long into a loss twice, and then submits one
// more order — the one whose recorded context says what the participant knew.
const twoLosersThenAnOrder = `praxis.market.v1,MNQ,50
time,sequence,session_id,bid,ask,bid_size,ask_size
3000,1,d1,20000,20001,10,10
4000,1,d1,19900,19901,10,10
5000,1,d1,19900,19901,10,10
6000,1,d1,19800,19801,10,10
7000,1,d1,19800,19801,10,10
`

// Scenario: what the screen says is what the journal says the trader knew
//
//	Given two completed losing trades and an order taken after them
//	Then the streak the interface serves is the streak that order recorded, and
//	  the equity the interface serves is the equity that order recorded.
//
// OrderContext documents both as what the trader knew, and the only thing that
// can make that true is the screen. The streak was declared in the state and
// never assigned, so the participant was shown zero while the journal recorded
// two on their next decision. The equity came from a second implementation of
// the marking rule that answered balance on any error — three separate paths —
// so a participant with an open position could be shown money they did not
// have. Neither breaks the accounting. Both falsify the sample, which is worse,
// because nothing downstream can tell.
func TestTheScreenAgreesWithWhatTheJournalSaysTheTraderKnew(t *testing.T) {
	dir := t.TempDir()
	marketPath := filepath.Join(dir, "market.csv")
	if err := os.WriteFile(marketPath, []byte(twoLosersThenAnOrder), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	journalPath := filepath.Join(dir, "journal.praxis")
	want, valued := writeTwoLosersThenAnOrder(t, marketPath, journalPath)

	server := open(t, marketPath, journalPath, session.Config{})
	resp := stateResponse(t, server)
	defer resp.Body.Close()
	raw, err := readAll(resp)
	if err != nil {
		t.Fatalf("readAll: %v", err)
	}

	var got struct {
		Money struct {
			BalanceCts string `json:"balanceCts"`
			EquityCts  string `json:"equityCts"`
		} `json:"money"`
		ConsecutiveLosingTrades uint32 `json:"consecutiveLosingTrades"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, raw)
	}

	if got.ConsecutiveLosingTrades != want.Context.ConsecutiveLosingTrades {
		t.Errorf("streak: the screen serves %d, the journal records %d on the order taken after it",
			got.ConsecutiveLosingTrades, want.Context.ConsecutiveLosingTrades)
	}
	// The fixture is built so the two counters differ. Without that a screen
	// serving ConsecutiveLosses instead would agree by coincidence.
	if want.Context.ConsecutiveLosingTrades != 1 || want.Context.ConsecutiveLosses != 2 {
		t.Fatalf("the fixture does not separate trades from legs: %d trades, %d legs",
			want.Context.ConsecutiveLosingTrades, want.Context.ConsecutiveLosses)
	}
	// Equity is compared against the last AccountValued and not against the
	// order's context, because the two are different moments: a context is
	// measured before the decision and the screen shows the account after it,
	// so a position the order just opened is worth a spread that the context
	// cannot contain. AccountValued is the session's own valuation at this
	// book, which is exactly what the screen must be showing.
	if got.Money.EquityCts != itoa64(int64(valued.EquityCts)) ||
		got.Money.BalanceCts != itoa64(int64(valued.BalanceCts)) {
		t.Errorf("money: the screen serves %s/%s, the journal's last valuation is %d/%d",
			got.Money.BalanceCts, got.Money.EquityCts, valued.BalanceCts, valued.EquityCts)
	}
	if valued.EquityCts == valued.BalanceCts {
		t.Fatal("the fixture left no open position, so the marking rule is untested")
	}
}

// itoa64 spells a figure the way the interface does. Money crosses as a string
// of whole cents, and a second spelling here would be the same defect this test
// exists to catch, one layer out.
func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

// writeTwoLosersThenAnOrder drives the fixture and returns the last order.
func writeTwoLosersThenAnOrder(t *testing.T, marketPath, journalPath string) (session.OrderSubmitted, session.AccountValued) {
	t.Helper()
	feed, err := marketdata.ReadFile(marketPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	w, err := persistence.OpenWriter(journalPath, persistence.DurableEveryBatch)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	defer w.Close()

	cfg := pilotConfig()
	cfg.Instrument = feed.Instrument
	s, err := session.New(cfg, feed.Observations[0].Quote.Time, w)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var elapsed session.ElapsedNanos
	tick := func() session.ElapsedNanos { elapsed += 1_000_000; return elapsed }
	act := func(n int) session.Decision {
		return session.Decision{
			GestureID:  "g-" + itoa64(int64(n)),
			AtUTCNanos: 1_764_000_000_000_000_000, Segment: 1, ElapsedNanos: tick(),
		}
	}
	confirm := func() {
		t.Helper()
		id, waiting := s.Pending(1)
		if !waiting {
			return
		}
		if err := s.AcknowledgePresentation(id, session.Instant{
			AtUTCNanos: 1_764_000_000_000_000_000, Segment: 1, ElapsedNanos: tick(),
		}); err != nil {
			t.Fatalf("AcknowledgePresentation: %v", err)
		}
	}

	// One losing trade abandoned in two pieces, then a fresh entry. That makes
	// ConsecutiveLosingTrades one and ConsecutiveLosses two, so a screen
	// serving the wrong counter is visible: they are different facts and
	// ADR-013 keeps both.
	sides := []market.Side{market.SideBuy, market.SideSell, market.SideSell, market.SideBuy, market.SideBuy}
	qtys := []market.Qty{2, 1, 1, 1, 1}
	for n := range feed.Observations {
		if err := marketdata.Drive(s, &marketdata.Feed{
			Instrument: feed.Instrument, Observations: feed.Observations[n : n+1],
		}, 0); err != nil {
			t.Fatalf("Drive row %d: %v", n, err)
		}
		confirm()
		o, err := market.NewMarketOrder("o-"+itoa64(int64(n+1)), feed.Instrument, sides[n], qtys[n])
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SubmitOrder(o, act(n+1)); err != nil {
			t.Fatalf("SubmitOrder row %d: %v", n, err)
		}
	}

	var (
		last   session.OrderSubmitted
		valued session.AccountValued
	)
	for _, e := range s.Events() {
		switch v := e.(type) {
		case session.OrderSubmitted:
			last = v
		case session.AccountValued:
			valued = v
		}
	}
	if last.Order.ID == "" {
		t.Fatal("no order in the journal")
	}
	return last, valued
}

package ui_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"praxis/internal/adapters/marketdata"
	"praxis/internal/adapters/persistence"
	"praxis/internal/market"
	"praxis/internal/session"
)

// thinBook shows ten on each side, so a fill of three leaves seven.
const thinBook = `praxis.market.v1,MNQ,50
time,sequence,session_id,bid,ask,bid_size,ask_size
3000,1,d1,20000,20001,10,10
`

// Scenario: the book on the screen is the book that is left
//
//	Given a journal in which the participant's own order took depth
//	When the interface is resumed on it
//	Then the size it serves is what remains, not what the file shows.
//
// One observation shows a finite book, and the session consumes the displayed
// size with every fill — that rule is why a simulator does not hand the same
// contracts to two orders. An interface reading the file rather than the
// session would show the participant depth their own order has already taken,
// and the next order they size against it would be sized against a quantity
// that is not there. It is the same principle as the streak and the equity:
// what the screen says is a claim the journal is already making.
func TestTheBookOnTheScreenIsTheBookThatIsLeft(t *testing.T) {
	dir := t.TempDir()
	marketPath := filepath.Join(dir, "market.csv")
	if err := os.WriteFile(marketPath, []byte(thinBook), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	journalPath := filepath.Join(dir, "journal.praxis")
	writeAFillThatTookDepth(t, marketPath, journalPath)

	server := open(t, marketPath, journalPath, session.Config{})
	resp := stateResponse(t, server)
	defer resp.Body.Close()
	raw, err := readAll(resp)
	if err != nil {
		t.Fatalf("readAll: %v", err)
	}

	var got struct {
		Book *struct {
			Bid     string `json:"bid"`
			Ask     string `json:"ask"`
			BidSize string `json:"bidSize"`
			AskSize string `json:"askSize"`
		} `json:"book"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, raw)
	}
	if got.Book == nil {
		t.Fatalf("no book on the screen: %s", raw)
	}

	// The file says ten offered. The order took three.
	if got.Book.AskSize != "7" {
		t.Errorf("askSize: the screen serves %s, the session has 7 left — the file's 10 is what the participant's own order already ate",
			got.Book.AskSize)
	}
	if got.Book.BidSize != "10" {
		t.Errorf("bidSize: got %s, want the untouched 10", got.Book.BidSize)
	}
	if got.Book.Bid != "20000" || got.Book.Ask != "20001" {
		t.Errorf("prices moved: %+v", *got.Book)
	}
}

// writeAFillThatTookDepth drives the one observation and buys three of the ten
// offered, so the journal it leaves has a partly eaten book.
func writeAFillThatTookDepth(t *testing.T, marketPath, journalPath string) {
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
	if err := marketdata.Drive(s, feed, 0); err != nil {
		t.Fatalf("Drive: %v", err)
	}
	presentIn(t, s, 1)
	buy, err := market.NewMarketOrder("o-1", feed.Instrument, market.SideBuy, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitOrder(buy, session.Decision{
		GestureID: "g-1", AtUTCNanos: 1_764_000_000_000_000_000, Segment: 1, ElapsedNanos: 1,
	}); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
}

// Scenario: a session that has seen nothing shows no book
//
//	Given a journal with no observation consumed
//	Then the screen carries no book at all.
//
// The zero quote is not a book of zeros: it is the absence of one, and a
// participant shown 0/0 with sizes of zero would be shown a market. Book is a
// pointer for the same reason Money is — absent rather than empty.
func TestASessionThatHasSeenNothingShowsNoBook(t *testing.T) {
	dir := t.TempDir()
	marketPath := filepath.Join(dir, "market.csv")
	if err := os.WriteFile(marketPath, []byte(thinBook), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	server := open(t, marketPath, filepath.Join(dir, "journal.praxis"), pilotConfig())
	resp := stateResponse(t, server)
	defer resp.Body.Close()
	raw, err := readAll(resp)
	if err != nil {
		t.Fatalf("readAll: %v", err)
	}

	var got struct {
		Book *json.RawMessage `json:"book"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, raw)
	}
	if got.Book != nil {
		t.Fatalf("a session that has observed nothing serves a book: %s", *got.Book)
	}
}

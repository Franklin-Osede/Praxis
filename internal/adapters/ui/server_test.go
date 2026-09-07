package ui_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"praxis/internal/adapters/marketdata"
	"praxis/internal/adapters/persistence"
	"praxis/internal/adapters/ui"
	"praxis/internal/challenge"
	"praxis/internal/market"
	"praxis/internal/session"
)

var mnq = market.Instrument{Symbol: "MNQ", CentsPerTick: 50}

const marketFile = `praxis.market.v1,MNQ,50
time,sequence,session_id,bid,ask,bid_size,ask_size
3000,1,d1,20000,20001,10,10
4000,2,d1,20010,20011,10,10
`

func pilotConfig() session.Config {
	return session.Config{
		Instrument:               mnq,
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

func paths(t *testing.T) (marketPath, journalPath string) {
	t.Helper()
	dir := t.TempDir()
	marketPath = filepath.Join(dir, "market.csv")
	if err := os.WriteFile(marketPath, []byte(marketFile), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return marketPath, filepath.Join(dir, "journal.praxis")
}

func open(t *testing.T, marketPath, journalPath string, cfg session.Config) *ui.Server {
	t.Helper()
	s, err := ui.Open(ui.Options{Market: marketPath, Journal: journalPath, New: cfg})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	go s.Serve()
	t.Cleanup(func() { s.Close() })
	return s
}

func post(t *testing.T, s *ui.Server, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+s.Addr()+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Origin", s.Origin())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

type controlBody struct {
	Lease   string   `json:"lease"`
	Segment string   `json:"segment"`
	State   ui.State `json:"state"`
}

func takeControl(t *testing.T, s *ui.Server) controlBody {
	t.Helper()
	resp := post(t, s, "/api/control")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("control: got %d", resp.StatusCode)
	}
	var body controlBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

// Scenario: two clients ask for the controls at once and one of them gets them
//
// A second tab is a second hand on the wheel. The loser is refused rather than
// queued, because control is not something to wait for: the tab should say so
// to whoever opened it, immediately.
func TestOnlyOneClientTakesTheControls(t *testing.T) {
	marketPath, journalPath := paths(t)
	s := open(t, marketPath, journalPath, pilotConfig())

	const clients = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
		refused int
	)
	wg.Add(clients)
	for i := 0; i < clients; i++ {
		go func() {
			defer wg.Done()
			resp := post(t, s, "/api/control")
			defer resp.Body.Close()
			mu.Lock()
			defer mu.Unlock()
			switch resp.StatusCode {
			case http.StatusOK:
				granted++
			case http.StatusConflict:
				refused++
			default:
				t.Errorf("unexpected status %d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()

	if granted != 1 || refused != clients-1 {
		t.Fatalf("granted %d and refused %d of %d", granted, refused, clients)
	}
}

// Scenario: handing control over invalidates the lease it took
//
// A request from the old lease may arrive after the transfer — sent before it,
// slow to land. It is refused on arrival rather than by when it was sent.
func TestATransferInvalidatesTheLeaseItReplaced(t *testing.T) {
	marketPath, journalPath := paths(t)
	s := open(t, marketPath, journalPath, pilotConfig())

	first := takeControl(t, s)
	second := post(t, s, "/api/control?transfer=yes")
	defer second.Body.Close()
	if second.StatusCode != http.StatusOK {
		t.Fatalf("transfer: got %d", second.StatusCode)
	}
	var took controlBody
	if err := json.NewDecoder(second.Body).Decode(&took); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if took.Lease == first.Lease {
		t.Fatal("the transfer handed back the same lease")
	}
	// And it opened a higher segment, because nothing carries across a change
	// of hands.
	if took.Segment <= first.Segment {
		t.Fatalf("segments: %s then %s, want the second higher", first.Segment, took.Segment)
	}
}

// Scenario: a restart continues above every segment the journal holds
func TestARestartOpensAHigherSegment(t *testing.T) {
	marketPath, journalPath := paths(t)

	// A journal with a decision already in it, taken in segment 3.
	writeDecisionInSegment(t, marketPath, journalPath, 3)

	s := open(t, marketPath, journalPath, session.Config{})
	if got := takeControl(t, s).Segment; got != "4" {
		t.Fatalf("segment: got %s, want 4 — above every segment the journal holds", got)
	}
}

// Scenario: a journal nobody traded cannot be driven by a person
//
// The pacing and the subject are one claim. An interface that quietly turned a
// scripted run into somebody's would be fabricating the sample, and the failure
// would be invisible afterwards.
func TestAScriptedJournalIsRefused(t *testing.T) {
	marketPath, journalPath := paths(t)

	scripted := pilotConfig()
	scripted.SubjectID, scripted.Pacing = "", session.PacingScripted
	writeJournal(t, marketPath, journalPath, scripted, false)

	_, err := ui.Open(ui.Options{Market: marketPath, Journal: journalPath})
	if !errors.Is(err, ui.ErrNotTraded) {
		t.Fatalf("got %v, want %v", err, ui.ErrNotTraded)
	}
}

// A journal claiming a cadence this interface cannot provide is refused too:
// serving it here would record it as confirmatory while a person advanced it by
// hand.
func TestAConfirmatoryJournalIsRefusedUntilThatModeExists(t *testing.T) {
	marketPath, journalPath := paths(t)
	cfg := pilotConfig()
	cfg.Pacing = session.PacingConfirmatory
	writeJournal(t, marketPath, journalPath, cfg, false)

	_, err := ui.Open(ui.Options{Market: marketPath, Journal: journalPath})
	if !errors.Is(err, ui.ErrConfirmatoryNotBuilt) {
		t.Fatalf("got %v, want %v", err, ui.ErrConfirmatoryNotBuilt)
	}
}

// Scenario: a journal whose format cannot record a decision is not continued
//
// It stays readable. What it cannot do is gain a human act, because the version
// it was written in has nowhere to put one.
func TestAnOlderJournalIsRefusedForHumanControl(t *testing.T) {
	marketPath, journalPath := paths(t)

	cfg := pilotConfig()
	cfg.SubjectID, cfg.Pacing = "", session.PacingScripted
	started := session.SessionStarted{
		Envelope: session.Envelope{Time: 1_000, Sequence: 1, Kind: session.KindSessionStarted},
		Config:   cfg,
	}
	writeRaw(t, journalPath, persistence.EventVersionV3, []session.Event{started})

	_, err := ui.Open(ui.Options{Market: marketPath, Journal: journalPath})
	if err == nil {
		t.Fatal("an unwritable journal was accepted")
	}
	// It is refused for one of the two reasons it must be: nobody traded it,
	// or its version has nowhere to record a decision. Both are true here and
	// either is a correct refusal.
	if !errors.Is(err, ui.ErrUnwritableVersion) && !errors.Is(err, ui.ErrNotTraded) {
		t.Fatalf("got %v", err)
	}
}

// Scenario: the state a participant sees carries nothing they may not know
//
// A field in the payload is available in the browser's developer tools whatever
// the stylesheet does, so for the experiment, sent is shown. A drawdown
// threshold here would change what a hypothesis about rule breaches is
// measuring — from "people break rules when already down" to "people react to a
// number they were shown".
func TestTheStateCarriesNothingTheParticipantMayNotKnow(t *testing.T) {
	marketPath, journalPath := paths(t)
	s := open(t, marketPath, journalPath, pilotConfig())

	resp := stateResponse(t, s)
	defer resp.Body.Close()
	raw, err := readAll(resp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	forbidden := []string{
		"maxDailyLoss", "MaxDailyLoss", "100000",
		"profitTarget", "ProfitTarget",
		"maxTotalLoss", "MaxTotalLoss", "200000",
		"trailing", "Trailing", "threshold", "Threshold",
		"highWater", "HighWater", "rules", "Rules",
	}
	for _, field := range forbidden {
		if strings.Contains(raw, field) {
			t.Fatalf("the state carries %q:\n%s", field, raw)
		}
	}

	// And it does carry the streak, because the journal claims the participant
	// knew it and only the screen can make that true.
	if !strings.Contains(raw, "consecutiveLosingTrades") {
		t.Fatalf("the state hides the streak the journal says was known:\n%s", raw)
	}
}

// Scenario: every quantity crosses as a decimal string
//
// JavaScript numbers are binary floating point. A monetary boundary that went
// through one would be a second money representation with its own rounding,
// which is the thing this project spends its integer discipline avoiding.
func TestEveryQuantityCrossesAsAString(t *testing.T) {
	marketPath, journalPath := paths(t)
	s := open(t, marketPath, journalPath, pilotConfig())

	resp := stateResponse(t, s)
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	money, ok := body["money"].(map[string]any)
	if !ok {
		t.Fatalf("no money in %+v", body)
	}
	for name, value := range money {
		if _, isString := value.(string); !isString {
			t.Fatalf("money.%s crossed as %T, want a decimal string", name, value)
		}
	}
	position, _ := body["position"].(map[string]any)
	if _, isString := position["netQty"].(string); !isString {
		t.Fatalf("position.netQty crossed as %T", position["netQty"])
	}
	if got := money["balanceCts"]; got != "5000000" {
		t.Fatalf("balance: got %v, want the exact cents as a string", got)
	}
}

// Scenario: the server is never reachable from off the machine
//
// "Only local" is not a boundary — a page open in the same browser can reach
// 127.0.0.1 — but binding every interface would put an unauthenticated kernel
// on the network, which is a different and worse problem.
func TestTheServerRefusesToLeaveLoopback(t *testing.T) {
	marketPath, journalPath := paths(t)
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.168.1.10:0"} {
		if _, err := ui.Open(ui.Options{
			Market: marketPath, Journal: journalPath, Addr: addr, New: pilotConfig(),
		}); err == nil {
			t.Fatalf("%q was accepted", addr)
		}
	}
}

// Scenario: a request from another origin is refused
//
// A malicious page open in the same browser can reach a local server. The
// origin it declares is what tells it apart from the interface's own page.
func TestARequestFromAnotherOriginIsRefused(t *testing.T) {
	marketPath, journalPath := paths(t)
	s := open(t, marketPath, journalPath, pilotConfig())

	req, err := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/api/control", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Origin", "http://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", resp.StatusCode)
	}

	// And a command sent as a GET is refused whatever its origin: a form or an
	// image tag can issue one with no script at all.
	get, err := http.Get("http://" + s.Addr() + "/api/control")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer get.Body.Close()
	if get.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d, want 405", get.StatusCode)
	}
}

// --- fixtures -------------------------------------------------------------

func stateResponse(t *testing.T, s *ui.Server) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+s.Addr()+"/api/state", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Origin", s.Origin())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

func readAll(resp *http.Response) (string, error) {
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

// writeJournal produces a journal the way the engine would, under a given
// configuration, and closes it.
func writeJournal(t *testing.T, marketPath, journalPath string, cfg session.Config, decide bool) {
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

	cfg.Instrument = feed.Instrument
	s, err := session.New(cfg, feed.Observations[0].Quote.Time, w)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := marketdata.Drive(s, feed, 0); err != nil {
		t.Fatalf("Drive: %v", err)
	}
	_ = decide
}

// writeDecisionInSegment produces a journal holding one human act, taken in the
// given segment, so that a restart has something to continue above.
func writeDecisionInSegment(t *testing.T, marketPath, journalPath string, segment uint64) {
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
	order, err := market.NewMarketOrder("o-1", feed.Instrument, market.SideBuy, 1)
	if err != nil {
		t.Fatalf("NewMarketOrder: %v", err)
	}
	act := session.Decision{
		GestureID: "g-1", AtUTCNanos: 1_764_000_000_000_000_000,
		Segment: segment, ElapsedNanos: 0,
	}
	if err := s.SubmitOrder(order, act); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
}

// writeRaw frames events in a chosen payload version, which is how a journal
// from an older version is produced now that the writer only writes the
// current one.
func writeRaw(t *testing.T, path, version string, events []session.Event) {
	t.Helper()
	out := persistence.HeaderFor(version)
	framed, err := persistence.EncodeBatch(1, events, version)
	if err != nil {
		t.Fatalf("EncodeBatch: %v", err)
	}
	if err := os.WriteFile(path, append(out, framed...), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

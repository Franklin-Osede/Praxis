package ui_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"praxis/internal/adapters/ui"
	"praxis/internal/session"
)

func postCommand(t *testing.T, s *ui.Server, body map[string]string) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/api/command", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", s.Origin())
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

// readyToTrade opens a server on a journal that has consumed the file, takes
// the controls, and confirms the observation on the screen — everything a human
// command needs before it can be taken.
func readyToTrade(t *testing.T) (*ui.Server, string, uint64) {
	t.Helper()
	marketPath, journalPath := paths(t)
	writeJournal(t, marketPath, journalPath, pilotConfig(), false)
	server := open(t, marketPath, journalPath, session.Config{})
	control := takeControl(t, server)

	segment, err := parseUint(control.Segment)
	if err != nil {
		t.Fatalf("segment %q: %v", control.Segment, err)
	}
	resp := acknowledge(t, server, map[string]string{
		"lease": control.Lease, "observedSequence": observedSequenceOf(t, server),
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("acknowledging: status %d, reason %q", resp.StatusCode, reasonOf(t, resp))
	}
	return server, control.Lease, segment
}

func parseUint(s string) (uint64, error) {
	var v uint64
	_, err := fmtSscan(s, &v)
	return v, err
}

// Scenario: an order is submitted, and submitting it again changes nothing
//
//	Given a client holding the controls, on a confirmed observation
//	When it submits an order and the response is lost, so it sends it again
//	Then the second answers 200 like the first, and the journal holds one
//	  order.
//
// A lost response is a retry. The handler recognises it by looking up what the
// gesture already committed, and never reaches the kernel — which refuses a
// repeated gesture without knowing whether it was the same command.
func TestAnOrderSubmittedTwiceUnderOneGestureIsOneOrder(t *testing.T) {
	server, lease, segment := readyToTrade(t)
	body := map[string]string{
		"kind": "submit_order", "lease": lease,
		"gesture": gestureName(segment, 1),
		"orderId": "o-1", "side": "buy", "type": "market", "qty": "2",
	}

	first := postCommand(t, server, body)
	defer first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first: status %d, reason %q", first.StatusCode, reasonOf(t, first))
	}
	after := netQtyOf(t, server)

	second := postCommand(t, server, body)
	defer second.Body.Close()
	if second.StatusCode != http.StatusOK {
		t.Fatalf("a retry was refused: status %d, reason %q", second.StatusCode, reasonOf(t, second))
	}
	if got := netQtyOf(t, server); got != after {
		t.Fatalf("position: %s after the retry, %s after the first — the retry executed", got, after)
	}
	if after != "2" {
		t.Fatalf("position: got %s, want 2", after)
	}
}

// Scenario: the same name for a different command is a conflict
//
//	Given a gesture already recorded
//	When the same name arrives commanding something else
//	Then it is refused as a conflict, not answered as a retry.
func TestOneNameForADifferentCommandIsAConflict(t *testing.T) {
	server, lease, segment := readyToTrade(t)
	name := gestureName(segment, 1)
	first := postCommand(t, server, map[string]string{
		"kind": "submit_order", "lease": lease,
		"gesture": name,
		"orderId": "o-1", "side": "buy", "type": "market", "qty": "2",
	})
	defer first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first: status %d, reason %q", first.StatusCode, reasonOf(t, first))
	}

	second := postCommand(t, server, map[string]string{
		"kind": "submit_order", "lease": lease,
		"gesture": name,
		"orderId": "o-2", "side": "sell", "type": "market", "qty": "5",
	})
	defer second.Body.Close()
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("status: got %d, want 409", second.StatusCode)
	}
	if got := reasonOf(t, second); got != "gesture_conflict" {
		t.Fatalf("reason: got %q, want gesture_conflict", got)
	}
}

// Scenario: an act must name the run it belongs to
//
//	Given a lease issued for one run
//	When an act arrives named for another
//	Then it is refused as belonging to a different run, and not as a reading
//	  out of order.
//
// The segment is on the decision and in the identifier, so it is checked rather
// than trusted. The value of the check is the message: without it the clock
// refuses the act for its reading, which is what a client whose lease was taken
// away would be told instead of being told that.
func TestAnActMustNameTheRunItBelongsTo(t *testing.T) {
	server, lease, segment := readyToTrade(t)
	resp := postCommand(t, server, map[string]string{
		"kind": "submit_order", "lease": lease,
		"gesture": gestureName(segment+7, 1),
		"orderId": "o-1", "side": "buy", "type": "market", "qty": "1",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status: got %d, want 409", resp.StatusCode)
	}
	if got := reasonOf(t, resp); got != "wrong_run" {
		t.Fatalf("reason: got %q, want wrong_run", got)
	}
}

// Scenario: two retries of one gesture race, and one order results
//
//	Given a client sending the same command twice at once — a browser retrying
//	  while the first request is still in flight
//	Then the journal holds one order.
//
// What this proves is idempotency under concurrency, which is real: eight
// clients asking for one act get one order.
//
// What it does NOT prove is the atomicity decision, and saying so is the point.
// Splitting the lookup from the execution into two turns of the loop survives
// this test 10 times out of 10, at 64 clients as well as at 8: the interleaving
// the split allows is not one the scheduler produces through an HTTP round trip
// often enough to be a test. That property is checked structurally instead, in
// TestOneCommandIsOneTurnOfTheLoop, where it is decidable — the loop is the
// only serialisation there is, so "cannot be interleaved" and "is one closure"
// are the same statement and the second is countable.
func TestTwoRetriesOfOneGestureLeaveOneOrder(t *testing.T) {
	server, lease, segment := readyToTrade(t)
	body := map[string]string{
		"kind": "submit_order", "lease": lease,
		"gesture": gestureName(segment, 1),
		"orderId": "o-1", "side": "buy", "type": "market", "qty": "1",
	}

	const clients = 8
	var wg sync.WaitGroup
	codes := make([]int, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			resp := postCommand(t, server, body)
			defer resp.Body.Close()
			codes[n] = resp.StatusCode
		}(i)
	}
	wg.Wait()

	for n, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("client %d: status %d", n, code)
		}
	}
	if got := netQtyOf(t, server); got != "1" {
		t.Fatalf("position: got %s from %d concurrent retries of one act, want 1", got, clients)
	}
}

// Scenario: an act's name is a run and a sequence, and both halves are needed
//
//	Given an identifier that is the run and nothing else
//	Then it is refused: without a sequence every act in a run is one name, and
//	  the second would be answered as a retry of the first.
func TestAnActsNameCarriesBothHalves(t *testing.T) {
	server, lease, segment := readyToTrade(t)
	for _, name := range []string{"1", "seven", ":1", "1:", "1:2:3"} {
		t.Run(name, func(t *testing.T) {
			resp := postCommand(t, server, map[string]string{
				"kind": "submit_order", "lease": lease,
				"gesture": name,
				"orderId": "o-1", "side": "buy", "type": "market", "qty": "1",
			})
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Fatalf("%q was accepted as an act's name", name)
			}
		})
	}
	_ = segment
}

// Scenario: an entry and its levels are one command
//
//	Given a submission carrying protective levels
//	Then the order and the protection are placed together, and the screen shows
//	  the protection.
//
// They are one decision and one durable batch — recording them separately would
// allow a journal in which the entry exists and its protection does not, a
// state the trader never chose. The handler must not turn one command into two,
// and the only thing standing between it and that is which method it calls.
func TestAnEntryAndItsLevelsAreOneCommand(t *testing.T) {
	server, lease, segment := readyToTrade(t)
	resp := postCommand(t, server, map[string]string{
		"kind": "submit_order", "lease": lease,
		"gesture": gestureName(segment, 1),
		"orderId": "e-1", "side": "buy", "type": "market", "qty": "1",
		"protectionStop": "19000", "protectionTarget": "21000",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, reason %q", resp.StatusCode, reasonOf(t, resp))
	}

	protection := protectionOf(t, server)
	if len(protection) != 1 {
		t.Fatalf("protection: got %d, want the one placed with the entry", len(protection))
	}
	if protection[0].StopPrice != "19000" || protection[0].TargetPrice != "21000" {
		t.Fatalf("levels: got %s/%s, want 19000/21000",
			protection[0].StopPrice, protection[0].TargetPrice)
	}
}

type shownProtection struct {
	Status      string `json:"status"`
	StopPrice   string `json:"stopPrice"`
	TargetPrice string `json:"targetPrice"`
}

func protectionOf(t *testing.T, s *ui.Server) []shownProtection {
	t.Helper()
	resp := stateResponse(t, s)
	defer resp.Body.Close()
	raw, err := readAll(resp)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Protection []shownProtection `json:"protection"`
	}
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatal(err)
	}
	return state.Protection
}

func netQtyOf(t *testing.T, s *ui.Server) string {
	t.Helper()
	resp := stateResponse(t, s)
	defer resp.Body.Close()
	raw, err := readAll(resp)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Position struct {
			NetQty string `json:"netQty"`
		} `json:"position"`
	}
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatal(err)
	}
	return state.Position.NetQty
}

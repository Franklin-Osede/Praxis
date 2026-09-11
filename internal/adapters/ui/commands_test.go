package ui_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"praxis/internal/adapters/ui"
)

// ok posts a command and insists it succeeded, so the assertions below are
// about what it did rather than about whether it ran.
func ok(t *testing.T, s *ui.Server, body map[string]string) {
	t.Helper()
	resp := postCommand(t, s, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status %d, reason %q", body["kind"], resp.StatusCode, reasonOf(t, resp))
	}
}

func workingOf(t *testing.T, s *ui.Server) []struct {
	ID         string `json:"id"`
	Side       string `json:"side"`
	Type       string `json:"type"`
	Qty        string `json:"qty"`
	LimitPrice string `json:"limitPrice"`
	StopPrice  string `json:"stopPrice"`
} {
	t.Helper()
	resp := stateResponse(t, s)
	defer resp.Body.Close()
	raw, err := readAll(resp)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Working []struct {
			ID         string `json:"id"`
			Side       string `json:"side"`
			Type       string `json:"type"`
			Qty        string `json:"qty"`
			LimitPrice string `json:"limitPrice"`
			StopPrice  string `json:"stopPrice"`
		} `json:"working"`
	}
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatal(err)
	}
	return state.Working
}

// Scenario: every order type crosses the boundary
//
//	Given a limit and a stop submitted through the interface
//	Then each rests carrying the price its type requires and no other.
//
// Only market orders had ever been submitted over HTTP, so the mapping from a
// tag to NewLimitOrder or NewStopOrder had never run — and neither had the
// canonical parsing of limitPrice and stopPrice. A crossed wiring there fails
// loudly, because Order.Validate refuses a limit carrying a stop price; the
// parsing does not, and a non-canonical price crossing this boundary was dead
// code a pilot could wake.
func TestEveryOrderTypeCrossesTheBoundary(t *testing.T) {
	server, lease, segment := readyToTrade(t)

	ok(t, server, map[string]string{
		"kind": "submit_order", "lease": lease, "gesture": gestureName(segment, 1),
		"orderId": "lim-1", "side": "buy", "type": "limit", "qty": "2", "limitPrice": "19000",
	})
	ok(t, server, map[string]string{
		"kind": "submit_order", "lease": lease, "gesture": gestureName(segment, 2),
		"orderId": "stp-1", "side": "buy", "type": "stop", "qty": "3", "stopPrice": "21000",
	})

	working := workingOf(t, server)
	if len(working) != 2 {
		t.Fatalf("working: got %d, want the limit and the stop", len(working))
	}
	lim, stp := working[0], working[1]
	if lim.ID != "lim-1" || lim.Type != "limit" || lim.Qty != "2" ||
		lim.LimitPrice != "19000" || lim.StopPrice != "0" {
		t.Fatalf("the limit crossed as %+v", lim)
	}
	if stp.ID != "stp-1" || stp.Type != "stop" || stp.Qty != "3" ||
		stp.StopPrice != "21000" || stp.LimitPrice != "0" {
		t.Fatalf("the stop crossed as %+v", stp)
	}

	t.Run("and a price that is not canonical is refused", func(t *testing.T) {
		resp := postCommand(t, server, map[string]string{
			"kind": "submit_order", "lease": lease, "gesture": gestureName(segment, 3),
			"orderId": "lim-2", "side": "buy", "type": "limit", "qty": "1", "limitPrice": "019000",
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status: got %d, want 400", resp.StatusCode)
		}
		if got := reasonOf(t, resp); got != "not_canonical" {
			t.Fatalf("reason: got %q, want not_canonical", got)
		}
	})
}

// Scenario: a trader withdraws an order they placed
//
//	Given a limit resting
//	When the trader cancels it
//	Then it stops working.
//
// cancel_order had never been sent. Its case in the switch called the right
// method by inspection only: wiring it to CancelProtection compiled and left
// every package green.
func TestCancellingAnOrderThroughTheInterface(t *testing.T) {
	server, lease, segment := readyToTrade(t)
	ok(t, server, map[string]string{
		"kind": "submit_order", "lease": lease, "gesture": gestureName(segment, 1),
		"orderId": "lim-1", "side": "buy", "type": "limit", "qty": "2", "limitPrice": "19000",
	})
	if len(workingOf(t, server)) != 1 {
		t.Fatal("the order did not rest, so cancelling it proves nothing")
	}

	ok(t, server, map[string]string{
		"kind": "cancel_order", "lease": lease, "gesture": gestureName(segment, 2),
		"orderId": "lim-1",
	})
	if working := workingOf(t, server); len(working) != 0 {
		t.Fatalf("working: got %+v, want the order gone", working)
	}
}

// Scenario: a protection is changed and withdrawn, by each name it can have
//
//	Given a protection planned against an entry that has not filled, and one
//	  active on an episode
//	Then a change and a withdrawal reach each of them.
//
// refOf had zero coverage: neither variant of the reference had ever been built
// from a body. ProtectionRef.Validate refuses the impossible combination, but
// the mapping from JSON to the union had never run at all, so an entry
// reference arriving as an episode — or either arriving as nothing — was
// untested in both directions.
func TestChangingAndWithdrawingAProtectionByEachName(t *testing.T) {
	t.Run("planned, named by its entry", func(t *testing.T) {
		server, lease, segment := readyToTrade(t)
		ok(t, server, map[string]string{
			"kind": "submit_order", "lease": lease, "gesture": gestureName(segment, 1),
			"orderId": "e-1", "side": "buy", "type": "limit", "qty": "1", "limitPrice": "19000",
			"protectionStop": "18000", "protectionTarget": "19500",
		})
		planned := protectionOf(t, server)
		if len(planned) != 1 || planned[0].Status != "planned" {
			t.Fatalf("protection: got %+v, want one planned", planned)
		}

		ok(t, server, map[string]string{
			"kind": "replace_protection", "lease": lease, "gesture": gestureName(segment, 2),
			"refKind": "entry", "refOrderId": "e-1",
			"protectionStop": "17500", "protectionTarget": "19500",
		})
		if got := protectionOf(t, server); len(got) != 1 || got[0].StopPrice != "17500" {
			t.Fatalf("after the change: got %+v, want the stop at 17500", got)
		}

		ok(t, server, map[string]string{
			"kind": "withdraw_protection", "lease": lease, "gesture": gestureName(segment, 3),
			"refKind": "entry", "refOrderId": "e-1",
		})
		if got := protectionOf(t, server); len(got) != 0 {
			t.Fatalf("after the withdrawal: got %+v, want none", got)
		}
	})

	t.Run("active, named by its episode", func(t *testing.T) {
		server, lease, segment := readyToTrade(t)
		ok(t, server, map[string]string{
			"kind": "submit_order", "lease": lease, "gesture": gestureName(segment, 1),
			"orderId": "e-1", "side": "buy", "type": "market", "qty": "1",
			"protectionStop": "19000", "protectionTarget": "21000",
		})
		active := protectionOf(t, server)
		if len(active) != 1 || active[0].Status != "active" {
			t.Fatalf("protection: got %+v, want one active", active)
		}
		episode := episodeIDOf(t, server)

		ok(t, server, map[string]string{
			"kind": "replace_protection", "lease": lease, "gesture": gestureName(segment, 2),
			"refKind": "episode", "refEpisodeId": episode,
			"protectionStop": "19500", "protectionTarget": "21000",
		})
		if got := protectionOf(t, server); len(got) != 1 || got[0].StopPrice != "19500" {
			t.Fatalf("after the change: got %+v, want the stop at 19500", got)
		}

		ok(t, server, map[string]string{
			"kind": "withdraw_protection", "lease": lease, "gesture": gestureName(segment, 3),
			"refKind": "episode", "refEpisodeId": episode,
		})
		if got := protectionOf(t, server); len(got) != 0 {
			t.Fatalf("after the withdrawal: got %+v, want none", got)
		}
	})

	t.Run("a reference that names neither is refused", func(t *testing.T) {
		server, lease, segment := readyToTrade(t)
		resp := postCommand(t, server, map[string]string{
			"kind": "withdraw_protection", "lease": lease, "gesture": gestureName(segment, 1),
			"refKind": "whatever", "refOrderId": "e-1",
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status: got %d, want 400", resp.StatusCode)
		}
	})
}

func episodeIDOf(t *testing.T, s *ui.Server) string {
	t.Helper()
	resp := stateResponse(t, s)
	defer resp.Body.Close()
	raw, err := readAll(resp)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Protection []struct {
			EpisodeID string `json:"episodeId"`
		} `json:"protection"`
	}
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Protection) == 0 || state.Protection[0].EpisodeID == "" {
		t.Fatalf("no episode to name:\n%s", raw)
	}
	return state.Protection[0].EpisodeID
}

// Scenario: every command is idempotent by name, not only a submission
//
//	Given each of the four human commands sent twice under one gesture
//	Then the second answers 200 and changes nothing.
//
// Only a submission had ever been retried, so the other three branches of the
// gesture the handler builds — the act a retry is compared against — were never
// exercised. A withdrawal declaring itself a replacement in that branch survives
// every other test: it calls the right method, so the journal is right, and only
// a retry notices that the act it is compared with is the wrong kind. Which is
// the one thing the whole gesture register exists to get right.
func TestEveryCommandIsIdempotentUnderItsName(t *testing.T) {
	retry := func(t *testing.T, s *ui.Server, body map[string]string) {
		t.Helper()
		ok(t, s, body)
		before := stateBody(t, s)
		resp := postCommand(t, s, body)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s retried: status %d, reason %q", body["kind"], resp.StatusCode, reasonOf(t, resp))
		}
		if after := stateBody(t, s); after != before {
			t.Fatalf("%s retried changed the session:\n before %s\n after  %s",
				body["kind"], before, after)
		}
	}

	t.Run("cancel_order", func(t *testing.T) {
		server, lease, segment := readyToTrade(t)
		ok(t, server, map[string]string{
			"kind": "submit_order", "lease": lease, "gesture": gestureName(segment, 1),
			"orderId": "lim-1", "side": "buy", "type": "limit", "qty": "1", "limitPrice": "19000",
		})
		retry(t, server, map[string]string{
			"kind": "cancel_order", "lease": lease, "gesture": gestureName(segment, 2),
			"orderId": "lim-1",
		})
	})

	t.Run("replace_protection", func(t *testing.T) {
		server, lease, segment := readyToTrade(t)
		ok(t, server, map[string]string{
			"kind": "submit_order", "lease": lease, "gesture": gestureName(segment, 1),
			"orderId": "e-1", "side": "buy", "type": "market", "qty": "1",
			"protectionStop": "19000", "protectionTarget": "21000",
		})
		retry(t, server, map[string]string{
			"kind": "replace_protection", "lease": lease, "gesture": gestureName(segment, 2),
			"refKind": "episode", "refEpisodeId": episodeIDOf(t, server),
			"protectionStop": "19500", "protectionTarget": "21000",
		})
	})

	t.Run("withdraw_protection", func(t *testing.T) {
		server, lease, segment := readyToTrade(t)
		ok(t, server, map[string]string{
			"kind": "submit_order", "lease": lease, "gesture": gestureName(segment, 1),
			"orderId": "e-1", "side": "buy", "type": "market", "qty": "1",
			"protectionStop": "19000", "protectionTarget": "21000",
		})
		retry(t, server, map[string]string{
			"kind": "withdraw_protection", "lease": lease, "gesture": gestureName(segment, 2),
			"refKind": "episode", "refEpisodeId": episodeIDOf(t, server),
		})
	})

	t.Run("submit_order with levels", func(t *testing.T) {
		server, lease, segment := readyToTrade(t)
		retry(t, server, map[string]string{
			"kind": "submit_order", "lease": lease, "gesture": gestureName(segment, 1),
			"orderId": "e-1", "side": "buy", "type": "market", "qty": "1",
			"protectionStop": "19000", "protectionTarget": "21000",
		})
	})
}

func stateBody(t *testing.T, s *ui.Server) string {
	t.Helper()
	resp := stateResponse(t, s)
	defer resp.Body.Close()
	raw, err := readAll(resp)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

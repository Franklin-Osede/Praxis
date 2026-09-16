package ui_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"praxis/internal/adapters/ui"
)

// A local server is still a server. Nothing about being on loopback bounds what
// a page open in the same browser can send it, and a body with no ceiling is a
// process the machine can be made to run out of memory. The refusal is its own:
// a request that is well formed and too large is not unreadable bytes, and an
// operator reading 400 would look for a client writing bad JSON.

// postBody sends a body to a route without going through any helper that
// assumes it is small.
func postBody(t *testing.T, s *ui.Server, route string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+s.Addr()+route, bytes.NewReader(body))
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

// oversized is a body that is valid JSON and far past any ceiling a command
// needs, so what refuses it is the size and not the shape.
func oversized(t *testing.T, field string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]string{field: strings.Repeat("x", 1<<20)})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Scenario: a body past the ceiling is refused as too large, on every route
// that reads one
func TestABodyPastTheCeilingIsRefused(t *testing.T) {
	marketPath, journalPath := paths(t)
	s := open(t, marketPath, journalPath, pilotConfig())

	for _, route := range []struct{ path, field string }{
		{"/api/command", "orderId"},
		{"/api/acknowledge", "observedSequence"},
		{"/api/step", "fromObservedSequence"},
		{"/api/control?transfer=yes", "handover"},
	} {
		t.Run(route.path, func(t *testing.T) {
			refused(t, postBody(t, s, route.path, oversized(t, route.field)),
				http.StatusRequestEntityTooLarge, "body_too_large")
		})
	}
}

// Scenario: a body that is small and malformed is still unreadable, not large
//
// The two refusals are different findings and the ceiling must not swallow one
// into the other.
func TestASmallMalformedBodyIsStillUnreadable(t *testing.T) {
	marketPath, journalPath := paths(t)
	s := open(t, marketPath, journalPath, pilotConfig())

	refused(t, postBody(t, s, "/api/command", []byte("{")), http.StatusBadRequest, "unreadable")
}

// Scenario: a body just under the ceiling is read
//
// Without this the ceiling could be anything, including zero, and every test
// above would still pass.
func TestABodyUnderTheCeilingIsRead(t *testing.T) {
	marketPath, journalPath := paths(t)
	s := open(t, marketPath, journalPath, pilotConfig())
	control := takeControl(t, s)

	// A command whose identifier is long but well inside the ceiling: it is
	// refused for what it says, not for its size.
	body, err := json.Marshal(map[string]string{
		"kind": "cancel_order", "lease": control.Lease,
		"gesture": gestureName(1, 1), "orderId": strings.Repeat("a", 8<<10),
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := postBody(t, s, "/api/command", body)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusRequestEntityTooLarge {
		t.Fatalf("a body of %d bytes was refused as too large", len(body))
	}
	if got := reasonOf(t, resp); got == "body_too_large" {
		t.Fatalf("reason: %q", got)
	}
}

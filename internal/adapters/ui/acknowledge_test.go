package ui_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"praxis/internal/adapters/ui"
	"praxis/internal/session"
)

func acknowledge(t *testing.T, s *ui.Server, body map[string]string) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/api/acknowledge", bytes.NewReader(raw))
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

func reasonOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body struct {
		Reason string `json:"reason"`
	}
	raw, err := readAll(resp)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, raw)
	}
	return body.Reason
}

// Scenario: confirming an observation, and confirming it again
//
//	Given a client holding the controls
//	When it confirms the observation on the screen, and then confirms it again
//	Then both answer 200 with the whole state, and the second records nothing.
//
// A lost response is a retry. It must not come back as an error in front of the
// participant, and the two answers are the same shape so that they are
// indistinguishable in fact and not only in status.
func TestAcknowledgingAnObservationAndAcknowledgingItAgain(t *testing.T) {
	marketPath, journalPath := paths(t)
	// A journal that already consumed the file, so there is an observation on
	// the screen to confirm.
	writeJournal(t, marketPath, journalPath, pilotConfig(), false)
	server := open(t, marketPath, journalPath, session.Config{})
	control := takeControl(t, server)

	body := map[string]string{
		"lease": control.Lease, "observedSequence": observedSequenceOf(t, server),
		"atUtcNanos": "1764000000000000000", "elapsedNanos": "1000000",
	}

	first := acknowledge(t, server, body)
	defer first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first: status %d, reason %q", first.StatusCode, reasonOf(t, first))
	}
	one, err := readAll(first)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(one)) {
		t.Fatalf("the body is not the state: %s", one)
	}

	second := acknowledge(t, server, body)
	defer second.Body.Close()
	if second.StatusCode != http.StatusOK {
		t.Fatalf("a retry was refused: status %d, reason %q", second.StatusCode, reasonOf(t, second))
	}
}

// Scenario: every refusal names itself
//
//	Given requests refused for different reasons
//	Then each carries a typed reason, and a client can tell them apart without
//	  reading a sentence.
func TestEveryRefusalCarriesATypedReason(t *testing.T) {
	marketPath, journalPath := paths(t)
	writeJournal(t, marketPath, journalPath, pilotConfig(), false)
	server := open(t, marketPath, journalPath, session.Config{})
	control := takeControl(t, server)
	observed := observedSequenceOf(t, server)

	for _, tc := range []struct {
		name   string
		body   map[string]string
		status int
		reason string
	}{
		{"a lease nobody holds", map[string]string{
			"lease": "not-the-lease", "observedSequence": observed,
			"atUtcNanos": "1764000000000000000", "elapsedNanos": "1000000",
		}, http.StatusConflict, "lease_stale"},

		{"an integer that is not canonical", map[string]string{
			"lease": control.Lease, "observedSequence": observed,
			"atUtcNanos": "+1764000000000000000", "elapsedNanos": "1000000",
		}, http.StatusBadRequest, "not_canonical"},

		{"a leading zero", map[string]string{
			"lease": control.Lease, "observedSequence": observed,
			"atUtcNanos": "1764000000000000000", "elapsedNanos": "01000000",
		}, http.StatusBadRequest, "not_canonical"},

		{"an observation that is not the one waiting", map[string]string{
			"lease": control.Lease, "observedSequence": "999",
			"atUtcNanos": "1764000000000000000", "elapsedNanos": "1000000",
		}, http.StatusUnprocessableEntity, "not_presented"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := acknowledge(t, server, tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status: got %d, want %d", resp.StatusCode, tc.status)
			}
			if got := reasonOf(t, resp); got != tc.reason {
				t.Fatalf("reason: got %q, want %q", got, tc.reason)
			}
		})
	}

	t.Run("an unreadable body", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "http://"+server.Addr()+"/api/acknowledge",
			bytes.NewReader([]byte("{not json")))
		req.Header.Set("Origin", server.Origin())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status: got %d, want 400", resp.StatusCode)
		}
		if got := reasonOf(t, resp); got != "unreadable" {
			t.Fatalf("reason: got %q, want unreadable", got)
		}
	})
}

// observedSequenceOf is the observation the screen is standing on, read from
// the state the server serves.
func observedSequenceOf(t *testing.T, s *ui.Server) string {
	t.Helper()
	resp := stateResponse(t, s)
	defer resp.Body.Close()
	raw, err := readAll(resp)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		ObservedSequence string `json:"observedSequence"`
	}
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatal(err)
	}
	if state.ObservedSequence == "" {
		t.Fatalf("the state names no observation, so nothing can be confirmed:\n%s", raw)
	}
	return state.ObservedSequence
}

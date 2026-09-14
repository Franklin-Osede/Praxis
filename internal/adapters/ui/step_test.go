package ui_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"praxis/internal/adapters/ui"
)

// Advancing is the one thing a person does that is not a command. It carries no
// gesture, so it is idempotent by content, the way the acknowledgement is: the
// body names the observation the step advances from. These tests hold the three
// ways that can go wrong — a retry that skips a row nobody saw, a stale step that
// moves the market under the screen, and a step taken before the screen was
// confirmed, which would leave a hole in the record of gaps between
// presentations that section 11 calls the data.

type stepped struct {
	Cursor           int    `json:"cursor"`
	Observations     int    `json:"observations"`
	ObservedSequence string `json:"observedSequence"`
}

func step(t *testing.T, s *ui.Server, lease, from string) *http.Response {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"lease": lease, "fromObservedSequence": from})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/api/step", bytes.NewReader(raw))
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

// stepOK advances and returns the state the server answered with.
func stepOK(t *testing.T, s *ui.Server, lease, from string) stepped {
	t.Helper()
	resp := step(t, s, lease, from)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step from %q: status %d, reason %q", from, resp.StatusCode, reasonOf(t, resp))
	}
	var body stepped
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

// confirm acknowledges the observation a step put on the screen.
func confirm(t *testing.T, s *ui.Server, lease, observed string) {
	t.Helper()
	resp := acknowledge(t, s, map[string]string{"lease": lease, "observedSequence": observed})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("acknowledging %s: status %d, reason %q", observed, resp.StatusCode, reasonOf(t, resp))
	}
}

// refused asserts a step is refused with the given status and reason.
func refused(t *testing.T, resp *http.Response, status int, reason string) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != status {
		t.Fatalf("status %d, want %d (reason %q)", resp.StatusCode, status, reasonOf(t, resp))
	}
	if got := reasonOf(t, resp); got != reason {
		t.Fatalf("reason %q, want %q", got, reason)
	}
}

// Scenario: a session nobody has advanced starts from its first row
//
// The defect this endpoint exists to close, as a test. Before it, a fresh
// session could not reach an observation, so nothing could be confirmed and no
// command could be taken — and every other test in this package began from a
// journal already driven to the end of its file, which is how nobody noticed.
func TestAFreshSessionAdvancesFromItsFirstRowToAnOrder(t *testing.T) {
	marketPath, journalPath := paths(t)
	s := open(t, marketPath, journalPath, pilotConfig())
	control := takeControl(t, s)
	if control.State.ObservedSequence != "" || control.State.Cursor != 0 {
		t.Fatalf("a fresh session is already standing on something: %+v", control.State)
	}

	shown := stepOK(t, s, control.Lease, "")
	if shown.Cursor != 1 || shown.ObservedSequence == "" {
		t.Fatalf("the first step did not put the first row on the screen: %+v", shown)
	}
	confirm(t, s, control.Lease, shown.ObservedSequence)

	segment, err := parseUint(control.Segment)
	if err != nil {
		t.Fatal(err)
	}
	resp := postCommand(t, s, map[string]string{
		"kind": "submit_order", "lease": control.Lease, "gesture": gestureName(segment, 1),
		"orderId": "e-1", "side": "buy", "type": "market", "qty": "1",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an order after the first step: status %d, reason %q", resp.StatusCode, reasonOf(t, resp))
	}
}

// Scenario: a step duplicated after the row it produced was confirmed
//
// The response to a step can be lost after the server applied it, and the
// duplicate can arrive late — after the client has already reloaded the state
// and confirmed the row. The duplicate carries the same body; applied again, it
// would put a row on the journal that the participant never saw and the journal
// would not say one was missed.
func TestALateDuplicateStepShowsTheSameRow(t *testing.T) {
	marketPath, journalPath := paths(t)
	s := open(t, marketPath, journalPath, pilotConfig())
	control := takeControl(t, s)

	first := stepOK(t, s, control.Lease, "")
	confirm(t, s, control.Lease, first.ObservedSequence)
	again := stepOK(t, s, control.Lease, "")

	if again != first {
		t.Fatalf("a retried step moved the screen:\n first %+v\n again %+v", first, again)
	}
}

// Scenario: a retry is answered before the confirmation is asked for
//
// The row a lost step put on the screen has not been confirmed — the client
// never saw it. Asking for the confirmation first would turn a lost response
// into a refusal the participant is shown for a step they already took, which
// is the same order the acknowledgement answers a retry in and for the same
// reason.
func TestAStepRetryIsAnsweredBeforeTheConfirmationIsAskedFor(t *testing.T) {
	marketPath, journalPath := paths(t)
	s := open(t, marketPath, journalPath, pilotConfig())
	control := takeControl(t, s)

	first := stepOK(t, s, control.Lease, "")
	if again := stepOK(t, s, control.Lease, ""); again != first {
		t.Fatalf("the retry was not answered with the state: %+v", again)
	}
}

// Scenario: a step taken before the screen was confirmed is refused
//
// Section 11 says the gap between two presentations is the record. A step that
// does not require the current row to have been confirmed would let a row pass
// through the journal with no presentation at all, and the record would have a
// hole in exactly the quantity it exists to measure.
func TestAStepBeforeTheRowOnScreenIsConfirmedIsRefused(t *testing.T) {
	marketPath, journalPath := paths(t)
	s := open(t, marketPath, journalPath, pilotConfig())
	control := takeControl(t, s)

	shown := stepOK(t, s, control.Lease, "")
	refused(t, step(t, s, control.Lease, shown.ObservedSequence), http.StatusUnprocessableEntity, "not_presented")

	if now := stepOK(t, s, control.Lease, ""); now.Cursor != 1 {
		t.Fatalf("a refused step moved the cursor to %d", now.Cursor)
	}
}

// Scenario: a step naming a row that is no longer on the screen is refused
//
// A stale tab, or a very late duplicate. It is not a retry — the last step did
// not start from it — so it is refused rather than answered, and nothing moves.
func TestAStepFromARowNoLongerOnScreenIsRefused(t *testing.T) {
	marketPath, journalPath := paths(t)
	s := open(t, marketPath, journalPath, pilotConfig())
	control := takeControl(t, s)

	first := stepOK(t, s, control.Lease, "")
	confirm(t, s, control.Lease, first.ObservedSequence)
	second := stepOK(t, s, control.Lease, first.ObservedSequence)
	confirm(t, s, control.Lease, second.ObservedSequence)

	refused(t, step(t, s, control.Lease, ""), http.StatusConflict, "stale_step")
}

// Scenario: a step past the last row is refused
func TestAStepPastTheLastRowIsRefused(t *testing.T) {
	marketPath, journalPath := paths(t)
	s := open(t, marketPath, journalPath, pilotConfig())
	control := takeControl(t, s)

	from := ""
	for {
		shown := stepOK(t, s, control.Lease, from)
		confirm(t, s, control.Lease, shown.ObservedSequence)
		from = shown.ObservedSequence
		if shown.Cursor == shown.Observations {
			break
		}
	}
	refused(t, step(t, s, control.Lease, from), http.StatusUnprocessableEntity, "feed_exhausted")
}

// Scenario: a step from a lease that was handed over is refused
func TestAStepFromAHandedOverLeaseIsRefused(t *testing.T) {
	marketPath, journalPath := paths(t)
	s := open(t, marketPath, journalPath, pilotConfig())
	old := takeControl(t, s)

	transfer := post(t, s, "/api/control?transfer=yes")
	transfer.Body.Close()
	if transfer.StatusCode != http.StatusOK {
		t.Fatalf("transfer: status %d", transfer.StatusCode)
	}

	refused(t, step(t, s, old.Lease, ""), http.StatusConflict, "lease_stale")
}

// Scenario: "0" is not a spelling of "nothing on the screen"
//
// Absent is the empty string, as it is for an optional price. No observation has
// sequence zero, so a zero that was accepted would be a second spelling of
// "none" — and a client that sent it would be retrying or advancing depending on
// which spelling it happened to use.
func TestZeroIsNotASpellingOfNothingOnScreen(t *testing.T) {
	marketPath, journalPath := paths(t)
	s := open(t, marketPath, journalPath, pilotConfig())
	control := takeControl(t, s)

	refused(t, step(t, s, control.Lease, "0"), http.StatusBadRequest, "not_canonical")
}

// Scenario: a restart after a segment that only advanced and confirmed
//
// With steps this is the ordinary segment, not a rare one: a person who looks
// and does not trade leaves presentations and no command. A restart that
// computed the highest segment from commands alone would grant that segment
// again, and its first confirmation would be refused as out of order — a
// journal nobody could continue.
func TestARestartContinuesAboveASegmentOfOnlyPresentations(t *testing.T) {
	marketPath, journalPath := paths(t)

	first, err := ui.Open(ui.Options{Market: marketPath, Journal: journalPath, New: pilotConfig(), Now: tickingClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	go first.Serve()
	control := takeControl(t, first)
	shown := stepOK(t, first, control.Lease, "")
	confirm(t, first, control.Lease, shown.ObservedSequence)
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := ui.Open(ui.Options{Market: marketPath, Journal: journalPath, Now: tickingClock()})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	go second.Serve()
	defer second.Close()

	resumed := takeControl(t, second)
	if resumed.Segment == control.Segment {
		t.Fatalf("a restart granted segment %s again", resumed.Segment)
	}
	confirm(t, second, resumed.Lease, resumed.State.ObservedSequence)
}

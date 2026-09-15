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

// Taking the controls from whoever holds them is the one way into a session that
// does not start from nothing, and its whole reason to exist is that the holder
// is gone — a closed tab keeps its lease, and nothing can present it. So it
// cannot ask for the current lease. What it asks for is a key only the operator's
// console shows, and that works once. Without one, any process on the machine
// could take the controls and its decisions would enter the subject's journal
// indistinguishable from theirs.

// handoverKeys collects every key the server hands the operator, in order.
type handoverKeys struct {
	mu   sync.Mutex
	keys []string
}

func (h *handoverKeys) record(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.keys = append(h.keys, key)
}

func (h *handoverKeys) latest(t *testing.T) string {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.keys) == 0 {
		t.Fatal("the server handed the operator no key")
	}
	return h.keys[len(h.keys)-1]
}

func (h *handoverKeys) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.keys)
}

// openWithHandover opens a server whose handover keys the test can read, as an
// operator reads them off the console.
func openWithHandover(t *testing.T, marketPath, journalPath string, cfg session.Config) (*ui.Server, *handoverKeys) {
	t.Helper()
	keys := &handoverKeys{}
	s, err := ui.Open(ui.Options{Market: marketPath, Journal: journalPath, New: cfg, Handover: keys.record})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	go s.Serve()
	t.Cleanup(func() { s.Close() })
	return s, keys
}

func transfer(t *testing.T, s *ui.Server, key string) *http.Response {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"handover": key})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/api/control?transfer=yes", bytes.NewReader(raw))
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

// stillControls asserts a lease is still the controller, by taking a step with
// it: the one thing a lease is for.
func stillControls(t *testing.T, s *ui.Server, lease string) {
	t.Helper()
	resp := step(t, s, lease, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the lease lost the controls: status %d, reason %q", resp.StatusCode, reasonOf(t, resp))
	}
}

// Scenario: a transfer with no key is refused, and the holder keeps the controls
func TestATransferWithoutTheHandoverKeyIsRefused(t *testing.T) {
	marketPath, journalPath := paths(t)
	s, _ := openWithHandover(t, marketPath, journalPath, pilotConfig())
	holder := takeControl(t, s)

	refused(t, transfer(t, s, ""), http.StatusForbidden, "handover_refused")
	stillControls(t, s, holder.Lease)
}

// Scenario: a transfer with the wrong key is refused, and the holder keeps the
// controls
func TestATransferWithAWrongHandoverKeyIsRefused(t *testing.T) {
	marketPath, journalPath := paths(t)
	s, keys := openWithHandover(t, marketPath, journalPath, pilotConfig())
	holder := takeControl(t, s)

	wrong := keys.latest(t) + "0"
	refused(t, transfer(t, s, wrong), http.StatusForbidden, "handover_refused")
	stillControls(t, s, holder.Lease)
}

// Scenario: a handover key works once, and the operator is handed the next one
//
// A key that stayed valid would be a key anyone who once saw the console could
// reuse for the rest of the run. So it is spent by the transfer that uses it,
// and the operator is shown its replacement.
func TestAHandoverKeyWorksOnceAndIsReplaced(t *testing.T) {
	marketPath, journalPath := paths(t)
	s, keys := openWithHandover(t, marketPath, journalPath, pilotConfig())
	takeControl(t, s)

	first := keys.latest(t)
	took := transfer(t, s, first)
	took.Body.Close()
	if took.StatusCode != http.StatusOK {
		t.Fatalf("a transfer with the operator's key: status %d", took.StatusCode)
	}

	next := keys.latest(t)
	if next == first {
		t.Fatal("the key that was used is still the key the operator is shown")
	}
	refused(t, transfer(t, s, first), http.StatusForbidden, "handover_refused")

	again := transfer(t, s, next)
	again.Body.Close()
	if again.StatusCode != http.StatusOK {
		t.Fatalf("a transfer with the replacement key: status %d", again.StatusCode)
	}
}

// Scenario: two transfers racing on one key grant one
func TestTwoTransfersRacingOnOneHandoverKeyGrantOne(t *testing.T) {
	marketPath, journalPath := paths(t)
	s, keys := openWithHandover(t, marketPath, journalPath, pilotConfig())
	takeControl(t, s)
	key := keys.latest(t)

	const clients = 8
	codes := make(chan int, clients)
	var wg sync.WaitGroup
	for n := 0; n < clients; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := transfer(t, s, key)
			resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	wg.Wait()
	close(codes)

	granted := 0
	for code := range codes {
		if code == http.StatusOK {
			granted++
		}
	}
	if granted != 1 {
		t.Fatalf("%d of %d transfers on one key were granted, want exactly 1", granted, clients)
	}
	if keys.count() != 2 {
		t.Fatalf("the operator was handed %d keys, want the first and one replacement", keys.count())
	}
}

// Scenario: a server nobody gave a way to show the key allows no transfer
//
// The key is minted whether or not anyone can read it, so leaving the hook unset
// fails closed: the controls can still be taken when nobody holds them, and never
// taken from someone who does.
func TestAServerWithNoWayToShowTheKeyAllowsNoTransfer(t *testing.T) {
	marketPath, journalPath := paths(t)
	s := open(t, marketPath, journalPath, pilotConfig())
	holder := takeControl(t, s)

	refused(t, transfer(t, s, ""), http.StatusForbidden, "handover_refused")
	stillControls(t, s, holder.Lease)
}

package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"praxis/internal/adapters/marketdata"
	"praxis/internal/market"
	"praxis/internal/session"
)

// The reasons are a closed set, and a closed set is only closed if every member
// of it can be produced. lease_held was declared on the first day and was
// unreachable until this commit: the one place that could emit it answered in
// plain text, so a client discriminating on it would have waited forever for a
// string that never came.
//
// So this test does not check a list someone maintains. It reads the constants
// out of command.go and demands that each one is either produced here — by
// classify, or by a handler answering a real request — or named below as
// unreachable with the reason it is. A reason added and never connected fails
// immediately, which is the thing that did not happen the first time.

// unreachable is every declared reason no test can produce, with why.
var unreachable = map[Reason]string{
	// crypto/rand failing is the only way to reach it, and there is no seam for
	// that: the alternative is injecting the randomness that mints a lease,
	// which would be machinery bought to reach one branch.
	ReasonLeaseNotMinted: "the operating system's randomness would have to fail",
}

// declaredReasons reads the constants from the source rather than a list kept by
// hand, so a reason declared and never connected cannot hide behind the list not
// having been updated.
func declaredReasons(t *testing.T) map[Reason]string {
	t.Helper()
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, "command.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing command.go: %v", err)
	}

	declared := map[Reason]string{}
	ast.Inspect(parsed, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		named, ok := spec.Type.(*ast.Ident)
		if !ok || named.Name != "Reason" {
			return true
		}
		for n, name := range spec.Names {
			literal, ok := spec.Values[n].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				t.Fatalf("%s is not a string literal", name.Name)
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatalf("%s: %v", name.Name, err)
			}
			declared[Reason(value)] = name.Name
		}
		return true
	})
	if len(declared) == 0 {
		t.Fatal("no reasons were found in command.go, so this test proves nothing")
	}
	return declared
}

// fromClassify is every reason classify turns an error into. It is the sentinel
// half of the set: one error per reason, and the reason it must produce.
func fromClassify(t *testing.T) map[Reason]bool {
	t.Helper()
	produced := map[Reason]bool{}
	for _, err := range []error{
		session.ErrSessionNeedsRecovery, errServerClosing, ErrHandoverRefused,
		ErrControllerActive, ErrStaleLease, session.ErrGestureReused, errWrongSegment,
		errStaleStep, errUnknownKind, market.ErrNotCanonicalInt, market.ErrEmptyOrderID,
		session.ErrInteractionOrder, session.ErrNotYetPresented, session.ErrChallengeEnded,
		session.ErrNoSessionOpen, marketdata.ErrFeedExhausted,
		errors.New("something the session refused"),
	} {
		_, body := classify(err)
		produced[body.Reason] = true
	}
	return produced
}

// fromHandlers is every reason a handler produces without classify: the guard's
// refusals, the ones about the lease itself, and the ones about the bytes.
func fromHandlers(t *testing.T) map[Reason]bool {
	t.Helper()
	produced := map[Reason]bool{}
	record := func(w *httptest.ResponseRecorder, wantStatus int, what string) {
		t.Helper()
		if w.Code != wantStatus {
			t.Fatalf("%s: status %d, want %d (%s)", what, w.Code, wantStatus, w.Body.String())
		}
		if got := w.Header().Get("Content-Type"); got != "application/json" {
			t.Fatalf("%s: content type %q, want application/json", what, got)
		}
		produced[reasonIn(t, w)] = true
	}

	// The guard, on a server that needs nothing else to answer.
	guarded := (&Server{origin: "http://127.0.0.1:1"}).guard(http.NotFoundHandler())

	foreign := httptest.NewRequest(http.MethodPost, "/api/command", nil)
	foreign.Header.Set("Origin", "http://evil.example")
	w := httptest.NewRecorder()
	guarded.ServeHTTP(w, foreign)
	record(w, http.StatusForbidden, "a request from another origin")

	w = httptest.NewRecorder()
	guarded.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/command", nil))
	record(w, http.StatusMethodNotAllowed, "a command asked for with GET")

	// A server whose loop is gone: every handler answers the same way, and it
	// is not the session that stopped.
	gone := &Server{commands: make(chan func()), done: make(chan struct{})}
	gone.lease = newLease(0, nil)
	close(gone.done)
	// Each with a body its handler reads to the end, so what refuses them is the
	// loop being gone and not the bytes: acknowledge names the observation it
	// confirms before it asks the loop for anything, which is the right order
	// and makes an empty body a different refusal.
	for _, handler := range []struct {
		name string
		body string
		call func(http.ResponseWriter, *http.Request)
	}{
		{"control", "", gone.handleControl},
		{"state", "", gone.handleState},
		{"acknowledge", `{"observedSequence":"1"}`, gone.handleAcknowledge},
		{"command", `{"kind":"cancel_order","orderId":"o-1"}`, gone.handleCommand},
		{"step", "{}", gone.handleStep},
	} {
		w := httptest.NewRecorder()
		handler.call(w, httptest.NewRequest(http.MethodPost, "/api/"+handler.name, bytes.NewReader([]byte(handler.body))))
		record(w, http.StatusServiceUnavailable, handler.name+" with no loop")
	}

	// Bytes that are not a body.
	running := openForReasons(t)
	w = httptest.NewRecorder()
	running.handleCommand(w, httptest.NewRequest(http.MethodPost, "/api/command", bytes.NewReader([]byte("{"))))
	record(w, http.StatusBadRequest, "a body that is not JSON")

	// The controls taken, and then asked for again.
	first := httptest.NewRecorder()
	running.handleControl(first, httptest.NewRequest(http.MethodPost, "/api/control", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("taking the controls: status %d", first.Code)
	}
	w = httptest.NewRecorder()
	running.handleControl(w, httptest.NewRequest(http.MethodPost, "/api/control", nil))
	record(w, http.StatusConflict, "a second client asking for the controls")

	// And taken from their holder without the operator's key.
	w = httptest.NewRecorder()
	running.handleControl(w, httptest.NewRequest(http.MethodPost, "/api/control?transfer=yes", bytes.NewReader([]byte("{}"))))
	record(w, http.StatusForbidden, "a transfer with no handover key")

	return produced
}

func reasonIn(t *testing.T, w *httptest.ResponseRecorder) Reason {
	t.Helper()
	var body refusal
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding %q: %v", w.Body.String(), err)
	}
	if body.Reason == "" {
		t.Fatalf("a refusal carries no reason: %s", w.Body.String())
	}
	return body.Reason
}

// openForReasons is a server with a loop, over a one-row file.
func openForReasons(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	marketPath := filepath.Join(dir, "market.csv")
	if err := os.WriteFile(marketPath, []byte("praxis.market.v1,MNQ,50\n"+
		"time,sequence,session_id,bid,ask,bid_size,ask_size\n"+
		"3000,1,d1,20000,20001,10,10\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	s, err := Open(Options{
		Market: marketPath, Journal: filepath.Join(dir, "journal.praxis"), New: internalPilotConfig(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// Scenario: every reason the protocol declares can be produced
func TestEveryDeclaredReasonIsReachable(t *testing.T) {
	declared := declaredReasons(t)
	produced := fromClassify(t)
	for reason := range fromHandlers(t) {
		produced[reason] = true
	}

	for reason, name := range declared {
		if produced[reason] {
			continue
		}
		if why, listed := unreachable[reason]; listed {
			t.Logf("%s (%q) is unreachable: %s", name, reason, why)
			continue
		}
		t.Errorf("%s (%q) is declared and nothing produces it", name, reason)
	}
	for reason := range produced {
		if _, ok := declared[reason]; !ok {
			t.Errorf("%q is answered and not declared in command.go", reason)
		}
	}
	for reason := range unreachable {
		if produced[reason] {
			t.Errorf("%q is listed as unreachable and something produced it", reason)
		}
	}
}

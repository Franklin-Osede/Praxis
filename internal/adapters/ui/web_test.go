package ui_test

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// The screen is apparatus. What it shows is the participant's information set,
// what it derives would not be on the inventory section 11 authorises, and a
// request it makes to anything outside this machine leaks the timing of a
// session. None of that is checkable by reading the file every time someone
// changes it, so the rules that can be checked crudely are checked crudely — and
// called crude, because a test that greps is a test that can be fooled and is
// still better than a rule kept only in a comment.

func fetchPage(t *testing.T, s interface{ Addr() string }, path string) *http.Response {
	t.Helper()
	resp, err := http.Get("http://" + s.Addr() + path)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return resp
}

// Scenario: the root serves the page compiled into the binary
//
// Not a copy of it from a directory: the bytes on disk in the source tree are
// the bytes the binary holds, and there is no path that reads anything else.
func TestTheRootServesTheEmbeddedPage(t *testing.T) {
	marketPath, journalPath := paths(t)
	s := open(t, marketPath, journalPath, pilotConfig())

	resp := fetchPage(t, s, "/")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("content type %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache control %q, want no-store", got)
	}

	served, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	onDisk, err := os.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(served) != string(onDisk) {
		t.Fatal("what the server sends is not the file in the tree")
	}
}

// Scenario: a path that is not the root is not a file to fetch
func TestOnlyTheRootIsServed(t *testing.T) {
	marketPath, journalPath := paths(t)
	s := open(t, marketPath, journalPath, pilotConfig())

	for _, path := range []string{"/index.html", "/web/index.html", "/../server.go"} {
		resp := fetchPage(t, s, path)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404", path, resp.StatusCode)
		}
	}
}

// Scenario: the page asks nothing of anything outside this machine
//
// A font or a stylesheet from a content network is a request leaving the
// participant's machine at the moment they are shown an observation, which is
// the timing of the session in somebody else's log.
func TestThePageFetchesNothingFromOutside(t *testing.T) {
	page := pageSource(t)
	for _, forbidden := range []string{"<script src=", "<link rel=\"stylesheet\"", "<link rel=stylesheet", "@import"} {
		if strings.Contains(page, forbidden) {
			t.Errorf("the page contains %q, which fetches something it did not compile with", forbidden)
		}
	}
	// And no absolute URL anywhere, which catches the same thing written another
	// way — an image, a fetch, a form action.
	for _, scheme := range []string{"http://", "https://", "//cdn", "src=\"//"} {
		if strings.Contains(page, scheme) {
			t.Errorf("the page contains %q", scheme)
		}
	}
}

// Scenario: the page does no arithmetic on what the server sends
//
// Every quantity crosses as a decimal string of its smallest unit. A toFixed in
// a browser is a second implementation of a rounding rule that will eventually
// disagree with the one in Go, and a figure the page derived would not be on the
// inventory section 11 authorises even though the payload never changed.
//
// This is a grep, and a grep is fooled by arithmetic spelled another way — and
// it fires on the words as well as the code, which is how the page's own comment
// about rounding had to be reworded. It is here because the rule is worth a
// crude check at the place it can fail, not because it is a proof.
func TestThePageDerivesNothing(t *testing.T) {
	page := pageSource(t)
	for _, forbidden := range []string{"parseFloat", "parseInt", "Number(", "toFixed", "BigInt", "Math."} {
		if strings.Contains(page, forbidden) {
			t.Errorf("the page contains %q: what it shows must be what it was sent", forbidden)
		}
	}
}

// Scenario: the page does not poll
//
// The state changes when the participant acts, and every route answers with the
// whole of it. A timer asking again would be the interface producing traffic
// nobody asked for, and in a study a second clock nobody recorded.
func TestThePageDoesNotPoll(t *testing.T) {
	page := pageSource(t)
	for _, forbidden := range []string{"setInterval", "setTimeout", "EventSource", "WebSocket"} {
		if strings.Contains(page, forbidden) {
			t.Errorf("the page contains %q", forbidden)
		}
	}
}

func pageSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return string(raw)
}

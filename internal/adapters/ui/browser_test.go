package ui_test

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"praxis/internal/adapters/ui"
)

// These tests run the page the server actually serves in a real browser. The
// defect they were written for lived only in the page: the server already
// refused to advance past a row the new segment had not confirmed, and every
// HTTP test that confirmed it by hand passed. Only the page's own code could
// show that the page never confirmed it.
//
// They skip in exactly one case: no Chrome or Chromium executable is found, and
// the environment has not demanded one. That is a hole worth saying out loud:
// on such a machine nothing guards the screen's recovery path, which is why a
// run that accepts a change sets PRAXIS_REQUIRE_BROWSER=1 and turns the absence
// into a failure. A browser that is found and then fails to start is a failure
// everywhere, because a broken configuration is not a missing browser.
//
// What they cannot do is see a paint. THE-PAINT-CLAIM stays with the operator.
//
// One handover test failed once under `-race ./...` and has not repeated. The
// message was lost, and with it the diagnosis, so settle now dumps what the
// page saw before it fails: the two failure modes — the page never reached the
// condition, and the page was refused and stopped — are told apart by that
// record and not by the timeout.
//
// The waits are wide, and that is insurance rather than an explanation. Load
// does not account for the episode: with polling cut to three seconds these
// pass on a machine running sixteen busy loops, so the normal wait is well
// under that and the one failure implies a stall beyond ten seconds — a
// browser that did not start, a refusal, or a hang. Widening cannot make any
// of those right, so the episode stays open.

// recorder is injected before the page's own script runs. It wraps fetch and
// watches the controls, so the order in which the page asked, heard back, and
// opened the controls can be read afterwards. It changes no behaviour of the
// page.
const recorder = `
window.__praxis = [];
(() => {
  const original = window.fetch.bind(window);
  window.fetch = async (input, init) => {
    const path = String(input);
    window.__praxis.push("request " + path);
    const response = await original(input, init);
    window.__praxis.push("response " + path + " " + response.status);
    return response;
  };
  document.addEventListener("DOMContentLoaded", () => {
    const controls = document.getElementById("controls");
    new MutationObserver(() => {
      window.__praxis.push(controls.disabled ? "controls disabled" : "controls enabled");
    }).observe(controls, { attributes: true, attributeFilter: ["disabled"] });
  });
})();
`

// untilSettled is true once the controls are open or the page has stopped, and
// says which.
const untilSettled = `(() => {
  const notice = document.getElementById("notice");
  if (notice.dataset.tone === "stop") return "stopped: " + notice.textContent;
  if (!document.getElementById("controls").disabled) return "enabled";
  return null;
})()`

// untilAdvanced is true once the page shows the second row with its controls
// open again, or has stopped.
const untilAdvanced = `(() => {
  const notice = document.getElementById("notice");
  if (notice.dataset.tone === "stop") return "stopped: " + notice.textContent;
  const open = !document.getElementById("controls").disabled;
  if (open && document.getElementById("cursor").textContent === "2 of 2") return "advanced";
  return null;
})()`

// browserPath finds the browser the tests run: PRAXIS_BROWSER if it is set,
// otherwise the first of the places chromedp itself looks. An explicit path
// that is not an executable is a misconfiguration and fails rather than skips.
func browserPath(t *testing.T) (string, bool) {
	t.Helper()
	if explicit := os.Getenv("PRAXIS_BROWSER"); explicit != "" {
		found, err := exec.LookPath(explicit)
		if err != nil {
			t.Fatalf("PRAXIS_BROWSER=%q is not an executable: %v", explicit, err)
		}
		return found, true
	}
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		}
	case "windows":
		candidates = []string{"chrome", "chrome.exe"}
	default:
		candidates = []string{
			"headless_shell", "headless-shell", "chromium", "chromium-browser",
			"google-chrome", "google-chrome-stable", "chrome",
		}
	}
	for _, candidate := range candidates {
		if found, err := exec.LookPath(candidate); err == nil {
			return found, true
		}
	}
	return "", false
}

func browser(t *testing.T, s *ui.Server) context.Context {
	t.Helper()
	path, found := browserPath(t)
	if !found {
		if os.Getenv("PRAXIS_REQUIRE_BROWSER") == "1" {
			t.Fatal("PRAXIS_REQUIRE_BROWSER=1 and no Chrome or Chromium executable was found; set PRAXIS_BROWSER to its path")
		}
		t.Skip("no Chrome or Chromium executable was found, so the screen's recovery path is unguarded here; set PRAXIS_REQUIRE_BROWSER=1 to make this a failure")
	}

	options := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(path))
	allocator, cancelAllocator := chromedp.NewExecAllocator(context.Background(), options...)
	t.Cleanup(cancelAllocator)
	ctx, cancelBrowser := chromedp.NewContext(allocator)
	t.Cleanup(cancelBrowser)
	ctx, cancelTimeout := context.WithTimeout(ctx, 90*time.Second)
	t.Cleanup(cancelTimeout)

	if err := chromedp.Run(ctx); err != nil {
		t.Fatalf("the browser at %s was found and did not start: %v", path, err)
	}
	err := chromedp.Run(ctx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(recorder).Do(ctx)
			return err
		}),
		chromedp.Navigate("http://"+s.Addr()+"/"),
		chromedp.WaitVisible("#take", chromedp.ByID),
	)
	if err != nil {
		t.Fatalf("loading the page: %v", err)
	}
	return ctx
}

// settle runs the actions, then waits for the page to reach the condition. A
// failure here is the one that cost a diagnosis once, so it says what the page
// was doing rather than only that it stopped doing it.
func settle(t *testing.T, ctx context.Context, condition string, actions ...chromedp.Action) string {
	t.Helper()
	var outcome string
	actions = append(actions, chromedp.Poll(condition, &outcome, chromedp.WithPollingTimeout(60*time.Second)))
	if err := chromedp.Run(ctx, actions...); err != nil {
		t.Fatalf("waiting for the page: %v\n%s", err, diagnose(ctx))
	}
	return outcome
}

// diagnose reports what the page saw. It is called when a test is already
// failing, so it never fails itself: anything it cannot read is said to be
// unreadable and the rest is still printed.
func diagnose(ctx context.Context) string {
	// The browser's own context may be the thing that expired, so the dump
	// gets a fresh deadline of its own.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	read := func(expression string) string {
		var out string
		if err := chromedp.Run(ctx, chromedp.Evaluate(expression, &out)); err != nil {
			return "unreadable: " + err.Error()
		}
		return out
	}
	// A fetch answers a promise, and an Evaluate that does not wait for one
	// reports the promise instead of the answer.
	awaited := func(expression string) string {
		var out string
		err := chromedp.Run(ctx, chromedp.Evaluate(expression, &out,
			func(p *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams { return p.WithAwaitPromise(true) }))
		if err != nil {
			return "unreadable: " + err.Error()
		}
		return out
	}
	var record []string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__praxis || []`, &record)); err != nil {
		record = []string{"unreadable: " + err.Error()}
	}
	return strings.Join([]string{
		"notice:   " + read(`document.getElementById("notice").textContent`),
		"tone:     " + read(`document.getElementById("notice").dataset.tone || "(none)"`),
		"controls: " + read(`document.getElementById("controls").disabled ? "disabled" : "open"`),
		"cursor:   " + read(`document.getElementById("cursor").textContent`),
		// Asked through the page, so a server that stopped answering is itself
		// part of the report.
		"state:    " + awaited(`fetch("/api/state").then((r) => r.status + " " + r.statusText)`),
		"page saw:\n  " + strings.Join(record, "\n  "),
	}, "\n")
}

// pageRecord reads what the recorder saw, in order.
func pageRecord(t *testing.T, ctx context.Context) []string {
	t.Helper()
	var log []string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__praxis`, &log)); err != nil {
		t.Fatalf("reading the page's record: %v", err)
	}
	return log
}

// requireAcknowledgedBeforeOpening fails unless the most recent opening of the
// controls was preceded by an answered acknowledgement since the opening before
// it.
func requireAcknowledgedBeforeOpening(t *testing.T, ctx context.Context) {
	t.Helper()
	log := pageRecord(t, ctx)
	last, previous := -1, -1
	for i, entry := range log {
		if entry == "controls enabled" {
			previous, last = last, i
		}
	}
	if last < 0 {
		t.Fatalf("the controls never opened:\n%s", strings.Join(log, "\n"))
	}
	for _, entry := range log[previous+1 : last] {
		if entry == "response /api/acknowledge 200" {
			return
		}
	}
	t.Fatalf("the controls opened before the row on the screen was acknowledged:\n%s", strings.Join(log, "\n"))
}

// Scenario: a fresh session's screen opens its controls with nothing to confirm
// and advances through the file
//
//	Given a pilot session that has shown nothing
//	When the page takes the controls
//	Then the controls open and no acknowledgement is sent, because no row is shown
//	And each of two advances shows its row, acknowledged before the controls
//	  reopen.
func TestTheScreenAdvancesAFreshSession(t *testing.T) {
	marketPath, journalPath := paths(t)
	s, err := ui.Open(ui.Options{Market: marketPath, Journal: journalPath, New: pilotConfig(), Now: tickingClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	go s.Serve()
	t.Cleanup(func() { s.Close() })

	ctx := browser(t, s)
	if got := settle(t, ctx, untilSettled, chromedp.Click("#take", chromedp.ByID)); got != "enabled" {
		t.Fatalf("taking the controls: %s", got)
	}
	var cursor string
	if err := chromedp.Run(ctx, chromedp.Text("#cursor", &cursor, chromedp.ByID)); err != nil {
		t.Fatalf("reading the cursor: %v", err)
	}
	if cursor != "0 of 2" {
		t.Fatalf("cursor %q before any advance, want 0 of 2", cursor)
	}
	if log := pageRecord(t, ctx); strings.Contains(strings.Join(log, "\n"), "request /api/acknowledge") {
		t.Fatalf("taking the controls of a session that has shown nothing acknowledged something:\n%s", strings.Join(log, "\n"))
	}

	const untilFirst = `(() => {
  const notice = document.getElementById("notice");
  if (notice.dataset.tone === "stop") return "stopped: " + notice.textContent;
  const open = !document.getElementById("controls").disabled;
  if (open && document.getElementById("cursor").textContent === "1 of 2") return "advanced";
  return null;
})()`
	if got := settle(t, ctx, untilFirst, chromedp.Click("#advance", chromedp.ByID)); got != "advanced" {
		t.Fatalf("the first advance: %s", got)
	}
	requireAcknowledgedBeforeOpening(t, ctx)
	if got := settle(t, ctx, untilAdvanced, chromedp.Click("#advance", chromedp.ByID)); got != "advanced" {
		t.Fatalf("the second advance: %s", got)
	}
	requireAcknowledgedBeforeOpening(t, ctx)
}

// Scenario: a restarted server's screen confirms the row it shows before the
// participant can act
//
//	Given a pilot session whose first row was stepped and confirmed
//	And the server restarted on its journal
//	When the page takes the controls
//	Then it acknowledges the row on the screen before the controls open
//	And advancing shows the next row rather than stopping as not presented.
func TestTheScreenContinuesAfterARestart(t *testing.T) {
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
	t.Cleanup(func() { second.Close() })

	ctx := browser(t, second)
	if got := settle(t, ctx, untilSettled, chromedp.Click("#take", chromedp.ByID)); got != "enabled" {
		t.Fatalf("taking the controls: %s", got)
	}
	requireAcknowledgedBeforeOpening(t, ctx)
	if got := settle(t, ctx, untilAdvanced, chromedp.Click("#advance", chromedp.ByID)); got != "advanced" {
		t.Fatalf("advancing after a restart: %s", got)
	}
}

// Scenario: a screen that takes the controls from a tab that is gone confirms
// the row it shows before the participant can act
//
//	Given a pilot session whose first row was stepped and confirmed by a tab
//	  that still holds the lease
//	When the page presents the operator's handover key
//	Then it acknowledges the row on the screen before the controls open
//	And advancing shows the next row rather than stopping as not presented.
func TestTheScreenContinuesAfterAHandover(t *testing.T) {
	marketPath, journalPath := paths(t)

	var (
		mu  sync.Mutex
		key string
	)
	s, err := ui.Open(ui.Options{
		Market: marketPath, Journal: journalPath, New: pilotConfig(), Now: tickingClock(),
		Handover: func(k string) { mu.Lock(); key = k; mu.Unlock() },
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	go s.Serve()
	t.Cleanup(func() { s.Close() })

	gone := takeControl(t, s)
	shown := stepOK(t, s, gone.Lease, "")
	confirm(t, s, gone.Lease, shown.ObservedSequence)

	mu.Lock()
	handover := key
	mu.Unlock()

	ctx := browser(t, s)
	got := settle(t, ctx, untilSettled,
		chromedp.SendKeys("#handover", handover, chromedp.ByID),
		chromedp.Click("#transfer", chromedp.ByID),
	)
	if got != "enabled" {
		t.Fatalf("taking the controls with the handover key: %s", got)
	}
	requireAcknowledgedBeforeOpening(t, ctx)
	if got := settle(t, ctx, untilAdvanced, chromedp.Click("#advance", chromedp.ByID)); got != "advanced" {
		t.Fatalf("advancing after a handover: %s", got)
	}
}


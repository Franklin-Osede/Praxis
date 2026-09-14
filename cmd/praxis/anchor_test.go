package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"praxis/internal/adapters/persistence"
	"praxis/internal/session"
)

// dropLastBatch cuts a journal at the boundary before its final batch.
//
// This is not truncateJournalForCLI with a different argument. That one drops
// bytes and leaves an unconfirmed tail, which the reader already reports; this
// one removes a whole batch and leaves a file that is valid, clean and
// provable — the cut nothing inside a journal can detect.
func dropLastBatch(t *testing.T, path string) {
	t.Helper()
	whole, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	at := bytes.LastIndex(whole, []byte("\nBATCH "))
	if at < 0 {
		t.Fatal("the journal carries fewer than two batches, so there is no boundary to cut at")
	}
	if err := os.WriteFile(path, whole[:at+1], 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// Scenario: an anchor is emitted and the journal it came from agrees with it
func TestStoreAnchorEmitsAReferenceVerifyAccepts(t *testing.T) {
	binary := buildPraxis(t)
	path := filepath.Join(t.TempDir(), "journal.praxis")
	writeJournalForCLI(t, path)

	out, code := runPraxis(t, binary, "store", "anchor", path)
	if code != exitClean {
		t.Fatalf("store anchor: exit %d\n%s", code, out)
	}
	reference := strings.TrimSpace(out)
	if !strings.HasPrefix(reference, "praxis.anchor.v1 ") {
		t.Fatalf("the anchor is not the format's spelling: %q", reference)
	}

	out, code = runPraxis(t, binary, "store", "verify", path, "--anchor", reference)
	if code != exitClean {
		t.Fatalf("store verify --anchor: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "anchored:  yes") {
		t.Fatalf("verify made no completeness claim:\n%s", out)
	}
}

// Scenario: one cut journal, two claims, two answers
//
// The contrast is the test, so it is one test and not two: the same file
// verifies clean on its own and fails against its anchor. An operator who ran
// only the first command would be told the journal is provable, and it is —
// which is exactly why the second line has to exist.
func TestACutJournalVerifiesCleanAloneAndFailsAgainstItsAnchor(t *testing.T) {
	binary := buildPraxis(t)
	path := filepath.Join(t.TempDir(), "journal.praxis")
	writeJournalForCLI(t, path)

	out, code := runPraxis(t, binary, "store", "anchor", path)
	if code != exitClean {
		t.Fatalf("store anchor: exit %d\n%s", code, out)
	}
	reference := strings.TrimSpace(out)

	dropLastBatch(t, path)

	out, code = runPraxis(t, binary, "store", "verify", path)
	if code != exitClean {
		t.Fatalf("the cut journal did not verify on its own: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "condition: clean") {
		t.Fatalf("the cut journal was not reported clean:\n%s", out)
	}
	if !strings.Contains(out, "anchored:  not checked") {
		t.Fatalf("verify did not say completeness went unevaluated:\n%s", out)
	}

	out, code = runPraxis(t, binary, "store", "verify", path, "--anchor", reference)
	if code == exitNotProvable {
		t.Fatalf("a provable journal missing acts was reported as unprovable:\n%s", out)
	}
	if code != exitAnchorMismatch {
		t.Fatalf("the cut journal passed its own anchor: exit %d\n%s", code, out)
	}
	// The history still proves. That the two findings keep separate codes is
	// the point: a script reading 5 and a script reading 6 learn different
	// things, and only one of them means "this could not have happened".
	if !strings.Contains(out, "proved:") || !strings.Contains(out, "anchored:  FAILED") {
		t.Fatalf("verify did not report both claims separately:\n%s", out)
	}
}

// Scenario: a reference that cannot be read is the reference's fault
//
// It exits fatal rather than not-provable, because nothing was proved or
// disproved about the journal — the same reason a malformed anchor is refused
// before the file is opened.
func TestVerifyRefusesAReferenceItCannotRead(t *testing.T) {
	binary := buildPraxis(t)
	path := filepath.Join(t.TempDir(), "journal.praxis")
	writeJournalForCLI(t, path)

	out, code := runPraxis(t, binary, "store", "verify", path, "--anchor", "praxis.anchor.v9 - 1 1 sha256:00")
	if code != exitFatal {
		t.Fatalf("verify with an unreadable reference: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "anchored:  not checked — the reference could not be read") {
		t.Fatalf("verify blamed the journal for a defect in the reference:\n%s", out)
	}
}

// Scenario: an anchor emitted for one journal fails against another
//
// Two journals written the same way are byte-identical today, so this uses two
// that are not: the second has a batch more. It is the case ErrAnchorUnreached
// and a wrong journal share, and the refusal deliberately does not claim which
// of the two happened.
func TestAnAnchorFromALongerJournalIsRefused(t *testing.T) {
	binary := buildPraxis(t)
	dir := t.TempDir()
	long := filepath.Join(dir, "long.praxis")
	short := filepath.Join(dir, "short.praxis")

	writeJournalForCLI(t, long)
	writeJournalForCLI(t, short)
	dropLastBatch(t, short)

	out, code := runPraxis(t, binary, "store", "anchor", long)
	if code != exitClean {
		t.Fatalf("store anchor: exit %d\n%s", code, out)
	}

	out, code = runPraxis(t, binary, "store", "verify", short, "--anchor", strings.TrimSpace(out))
	if code != exitAnchorMismatch {
		t.Fatalf("a shorter journal accepted a longer journal's anchor: exit %d\n%s", code, out)
	}
}

// Scenario: the two claims describe one file, not two
//
// verify makes both findings under a single lock, so nothing can change the
// journal between them. The test cannot open the window it is guarding against
// — a race is not a thing to assert — so it asserts the property that closes
// it: while a writer holds the journal, verify reports busy rather than
// reporting on bytes it read before the lock was taken.
func TestVerifyWithAnAnchorTakesTheJournalOnce(t *testing.T) {
	binary := buildPraxis(t)
	path := filepath.Join(t.TempDir(), "journal.praxis")
	writeJournalForCLI(t, path)

	out, code := runPraxis(t, binary, "store", "anchor", path)
	if code != exitClean {
		t.Fatalf("store anchor: exit %d\n%s", code, out)
	}
	reference := strings.TrimSpace(out)

	release := holdJournal(t, path)
	defer release()

	if out, code := runPraxis(t, binary, "store", "verify", path, "--anchor", reference); code != exitBusy {
		t.Fatalf("verify read a journal another process was holding: exit %d\n%s", code, out)
	}
}

// forgeAValuation rewrites one recorded valuation and reframes every batch, so
// the file's checksums are all correct and its history is not.
//
// This is the forgery ADR-012 names as the thing inspection cannot catch: the
// bytes are exactly the bytes that were written, and what refuses them is
// replaying the log against the account that would have had to produce it.
func forgeAValuation(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	journal, err := persistence.ReadJournal(f)
	f.Close()
	if err != nil {
		t.Fatalf("ReadJournal: %v", err)
	}

	rebuilt, forged := persistence.Header(), false
	for _, b := range journal.Batches {
		events := append([]session.Event{}, b.Events...)
		for n, e := range events {
			valued, ok := e.(session.AccountValued)
			if !ok || forged {
				continue
			}
			valued.BalanceCts += 1
			events[n], forged = valued, true
		}
		framed, err := persistence.EncodeBatch(b.Number, events, journal.PayloadVersion)
		if err != nil {
			t.Fatalf("EncodeBatch: %v", err)
		}
		rebuilt = append(rebuilt, framed...)
	}
	if !forged {
		t.Fatal("the journal carries no valuation to forge")
	}
	if err := os.WriteFile(path, rebuilt, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// Scenario: a journal that fails both claims is told about both
//
// The completeness claim is computed before Verify and Replay so that an
// unprovable history cannot erase it. This is the test that the early return
// does not throw it away — without it, passing --anchor here produced output
// byte-identical to passing nothing at all.
func TestAJournalFailingBothClaimsIsToldAboutBoth(t *testing.T) {
	binary := buildPraxis(t)
	path := filepath.Join(t.TempDir(), "journal.praxis")
	writeJournalForCLI(t, path)

	out, code := runPraxis(t, binary, "store", "anchor", path)
	if code != exitClean {
		t.Fatalf("store anchor: exit %d\n%s", code, out)
	}
	reference := strings.TrimSpace(out)

	forgeAValuation(t, path)

	bare, bareCode := runPraxis(t, binary, "store", "verify", path)
	if bareCode != exitNotProvable {
		t.Fatalf("the forged journal proved: exit %d\n%s", bareCode, bare)
	}

	withAnchor, code := runPraxis(t, binary, "store", "verify", path, "--anchor", reference)
	// Precedence is settled: the unprovable history names the code.
	if code != exitNotProvable {
		t.Fatalf("a forged journal did not report its history first: exit %d\n%s", code, withAnchor)
	}
	// But the second finding is reported, and this is the regression: supplying
	// an anchor used to change nothing at all.
	if withAnchor == bare {
		t.Fatalf("--anchor changed nothing on an unprovable journal:\n%s", withAnchor)
	}
	if !strings.Contains(withAnchor, "anchored:  FAILED") {
		t.Fatalf("the completeness claim was not reported:\n%s", withAnchor)
	}
	if !strings.Contains(withAnchor, "fails both claims") {
		t.Fatalf("the output does not say the exit code names only one finding:\n%s", withAnchor)
	}
}

// Scenario: an unprovable journal with no anchor still says completeness went
// unevaluated
//
// The milder half of the same defect: this branch printed nothing at all about
// the second claim, so a journal whose history failed was the one case where
// the disclaimer went missing.
func TestAnUnprovableJournalStillSaysCompletenessWasNotChecked(t *testing.T) {
	binary := buildPraxis(t)
	path := filepath.Join(t.TempDir(), "journal.praxis")
	writeJournalForCLI(t, path)
	forgeAValuation(t, path)

	out, code := runPraxis(t, binary, "store", "verify", path)
	if code != exitNotProvable {
		t.Fatalf("the forged journal proved: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "anchored:  not checked") {
		t.Fatalf("no completeness line on the unprovable path:\n%s", out)
	}
}

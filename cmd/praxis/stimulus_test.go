package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"praxis/internal/adapters/persistence"
	"praxis/internal/session"
)

// The market file is bound transitively: the journal's anchor fixes the
// observations in its prefix, and Consumed compares every row of the supplied
// file with them. These tests hold that, and hold the two ways a second digest
// over the file's bytes went wrong — detecting nothing new where it was right,
// and accusing an honest file where the bytes changed and the prices did not.
// Where a finding changes the answer, they compare two invocations rather than
// read one.

// substituted is marketFile with one displayed size changed inside the rows a
// full run consumes. The quote stays valid, so only a comparison can object.
var substituted = strings.Replace(marketFile,
	"4000,1,d1,20010,20011,10,10", "4000,1,d1,20010,20011,10,11", 1)

// replayed runs the real binary over marketFile and returns the market file and
// the journal it produced.
func replayed(t *testing.T, binary string) (dir, marketPath, journal string) {
	t.Helper()
	dir = t.TempDir()
	marketPath = writeMarket(t, dir, "market.csv", marketFile)
	journal = filepath.Join(dir, "journal.praxis")
	if out, code := runPraxis(t, binary, replayArgs(marketPath, journal)...); code != exitClean {
		t.Fatalf("replay: exit %d\n%s", code, out)
	}
	return dir, marketPath, journal
}

func anchorText(t *testing.T, binary, journal string) string {
	t.Helper()
	out, code := runPraxis(t, binary, "store", "anchor", journal)
	if code != exitClean {
		t.Fatalf("store anchor: exit %d\n%s", code, out)
	}
	return strings.TrimSpace(out)
}

// forgeTheSubstitutedQuote rewrites the journal's record of the row substituted
// changes, the same way, and reframes every batch. After it the journal and the
// substituted file describe each other perfectly.
func forgeTheSubstitutedQuote(t *testing.T, path string) {
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
			observed, ok := e.(session.MarketObserved)
			if !ok || observed.Quote.Time != 4000 || observed.SourceSequence != 1 {
				continue
			}
			observed.Quote.AskSize++
			events[n], forged = observed, true
		}
		framed, err := persistence.EncodeBatch(b.Number, events, journal.PayloadVersion)
		if err != nil {
			t.Fatalf("EncodeBatch: %v", err)
		}
		rebuilt = append(rebuilt, framed...)
	}
	if !forged {
		t.Fatal("the journal carries no record of the substituted row")
	}
	if err := os.WriteFile(path, rebuilt, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// Scenario: a market file whose rows are not the ones the journal recorded
func TestASubstitutedMarketFileExitsSeven(t *testing.T) {
	binary := buildPraxis(t)
	dir, marketPath, journal := replayed(t, binary)

	honest, honestCode := runPraxis(t, binary, "store", "verify", journal, marketPath)
	writeMarket(t, dir, "market.csv", substituted)
	swapped, code := runPraxis(t, binary, "store", "verify", journal, marketPath)

	if honestCode != exitClean {
		t.Fatalf("the honest file did not verify: exit %d\n%s", honestCode, honest)
	}
	if swapped == honest {
		t.Fatalf("substituting the market file changed nothing:\n%s", swapped)
	}
	if code != exitStimulusMismatch || !strings.Contains(swapped, "stimulus:  FAILED") {
		t.Fatalf("a substituted market file: exit %d\n%s", code, swapped)
	}
}

// Scenario: a substitution made consistently in both files
//
// ADR-015's adversary. The row changes in the CSV and in the journal's record of
// it, so the two agree and Consumed passes. Nothing about the pair can object;
// the journal's anchor does, because the rewritten observation is inside the
// prefix it digests. This is what a second digest over the market file was built
// to catch, caught without one.
func TestASubstitutionMadeInBothFilesIsCaughtByTheJournalAnchor(t *testing.T) {
	binary := buildPraxis(t)
	dir, marketPath, journal := replayed(t, binary)
	reference := anchorText(t, binary, journal)

	forgeTheSubstitutedQuote(t, journal)
	writeMarket(t, dir, "market.csv", substituted)

	unanchored, unanchoredCode := runPraxis(t, binary, "store", "verify", journal, marketPath)
	anchored, code := runPraxis(t, binary, "store", "verify", journal, marketPath, "--anchor", reference)

	if unanchoredCode != exitClean || !strings.Contains(unanchored, "stimulus:  corresponds") {
		t.Fatalf("the premise does not hold: the rewritten pair should agree: exit %d\n%s",
			unanchoredCode, unanchored)
	}
	if code != exitAnchorMismatch || !strings.Contains(anchored, "anchored:  FAILED") {
		t.Fatalf("a consistent substitution against the journal's anchor: exit %d, want %d\n%s",
			code, exitAnchorMismatch, anchored)
	}
	// The pair still corresponds; what the line must not do is say the anchor
	// binds it, because the anchor is the thing that just failed.
	if strings.Contains(anchored, "bound through the anchor") {
		t.Fatalf("correspondence was credited to an anchor that failed:\n%s", anchored)
	}
}

// Scenario: the same rows in other bytes are the same stimulus
//
// The false accusation, as a regression. A market file with CRLF line endings
// parses to identical rows, so every price a decision was taken against is
// unchanged — and a digest over the bytes called it a substituted file. Nothing
// about the answer may change, so the test is that the two outputs are equal.
func TestTheSameRowsInOtherBytesAreTheSameStimulus(t *testing.T) {
	binary := buildPraxis(t)
	dir, marketPath, journal := replayed(t, binary)
	reference := anchorText(t, binary, journal)

	lf, lfCode := runPraxis(t, binary, "store", "verify", journal, marketPath, "--anchor", reference)
	writeMarket(t, dir, "market.csv", strings.ReplaceAll(marketFile, "\n", "\r\n"))
	crlf, crlfCode := runPraxis(t, binary, "store", "verify", journal, marketPath, "--anchor", reference)

	if lfCode != exitClean {
		t.Fatalf("the original file did not verify: exit %d\n%s", lfCode, lf)
	}
	if crlfCode != lfCode || crlf != lf {
		t.Fatalf("respelling the file without changing a row changed the answer: exit %d\n%s", crlfCode, crlf)
	}
}

// Scenario: a market file that grew after the run still verifies
func TestAGrownMarketFileStillVerifies(t *testing.T) {
	binary := buildPraxis(t)
	dir, marketPath, journal := replayed(t, binary)
	reference := anchorText(t, binary, journal)

	writeMarket(t, dir, "market.csv", marketFile+"7000,1,d2,19980,19981,10,10\n")

	out, code := runPraxis(t, binary, "store", "verify", journal, marketPath, "--anchor", reference)
	if code != exitClean {
		t.Fatalf("a grown market file was refused: exit %d\n%s", code, out)
	}
}

// Scenario: correspondence says what binds it
//
// "Corresponds" alone would read as "authentic". With an anchor that held, the
// consumed rows are bound through it; without one, the two files could have been
// written together. The two invocations must not read alike.
func TestCorrespondenceSaysWhatBindsIt(t *testing.T) {
	binary := buildPraxis(t)
	_, marketPath, journal := replayed(t, binary)
	reference := anchorText(t, binary, journal)

	bare, bareCode := runPraxis(t, binary, "store", "verify", journal, marketPath)
	bound, boundCode := runPraxis(t, binary, "store", "verify", journal, marketPath, "--anchor", reference)

	if bareCode != exitClean || boundCode != exitClean {
		t.Fatalf("verify: exits %d and %d\n%s\n%s", bareCode, boundCode, bare, bound)
	}
	if bare == bound {
		t.Fatalf("correspondence read the same with and without an anchor:\n%s", bare)
	}
	if !strings.Contains(bare, "would pass") || !strings.Contains(bound, "bound through the anchor") {
		t.Fatalf("correspondence was not qualified by what binds it:\n%s\n---\n%s", bare, bound)
	}
}

// Scenario: a stimulus finding outranks a cut journal
func TestAStimulusFindingOutranksACutJournal(t *testing.T) {
	binary := buildPraxis(t)
	dir, marketPath, journal := replayed(t, binary)
	reference := anchorText(t, binary, journal)

	dropLastBatch(t, journal)
	cutOnly, cutCode := runPraxis(t, binary, "store", "verify", journal, marketPath, "--anchor", reference)
	writeMarket(t, dir, "market.csv", substituted)
	both, code := runPraxis(t, binary, "store", "verify", journal, marketPath, "--anchor", reference)

	if cutCode != exitAnchorMismatch {
		t.Fatalf("a cut journal alone: exit %d, want %d\n%s", cutCode, exitAnchorMismatch, cutOnly)
	}
	if code != exitStimulusMismatch {
		t.Fatalf("a cut journal over a substituted file: exit %d, want %d\n%s", code, exitStimulusMismatch, both)
	}
	if !strings.Contains(both, "anchored:  FAILED") || !strings.Contains(both, "stimulus:  FAILED") {
		t.Fatalf("both findings were not reported:\n%s", both)
	}
}

// Scenario: an impossible history outranks a substituted file
func TestAnUnprovableHistoryOutranksASubstitutedFile(t *testing.T) {
	binary := buildPraxis(t)
	dir, marketPath, journal := replayed(t, binary)
	reference := anchorText(t, binary, journal)

	forgeAValuation(t, journal)
	writeMarket(t, dir, "market.csv", substituted)

	out, code := runPraxis(t, binary, "store", "verify", journal, marketPath, "--anchor", reference)
	if code != exitNotProvable {
		t.Fatalf("a forged history over a substituted file: exit %d, want %d\n%s", code, exitNotProvable, out)
	}
	if !strings.Contains(out, "stimulus:  FAILED") {
		t.Fatalf("the stimulus finding was dropped on the unprovable path:\n%s", out)
	}
}

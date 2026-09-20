package persistence

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// A repair discards bytes, and which bytes it discards is the operator's
// decision. RepairOptions splits that decision in two: an incomplete tail is
// discarded on Apply alone, because ConditionIncompleteTail says in so many
// words that "those bytes were never confirmed ... so discarding them loses
// nothing anyone believed had happened"; a batch that is whole on disk needs
// DiscardCorruptBatch on top, because it may have been reported as confirmed.
//
// These scenarios are about the sentence, not the truncation. The reader has
// exactly one thing to go on when it tells those two endings apart — whether
// enough bytes remain to cover the length the header claims — and a length
// that has been damaged upwards fails that test for the same reason a cut
// short file does. Everything after it, confirmed batches included, is then
// reported as an unfinished append and discarded on Apply alone.

// headerOffsets is where each batch's header line begins, derived from the
// batches the reader confirmed rather than by searching for a keyword: a
// journal's own reader is the only thing that knows where its frames are.
func headerOffsets(t *testing.T, raw []byte) []int64 {
	t.Helper()
	j := readJournalBytes(t, raw)
	offsets := make([]int64, 0, len(j.Batches))
	at := int64(0)
	for n, c := range raw {
		if c == '\n' {
			at = int64(n) + 1
			break
		}
	}
	for _, b := range j.Batches {
		offsets = append(offsets, at)
		at = b.EndOffset
	}
	return offsets
}

func readJournalBytes(t *testing.T, raw []byte) *Journal {
	t.Helper()
	j, err := ReadJournal(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadJournal: %v", err)
	}
	return j
}

// lengthFieldAt is where a header line's length field lies: "BATCH " and then
// the 20-digit batch number and its separator. formatMetadata is the one
// definition of that layout, and this is the only other place that depends on
// it.
func lengthFieldAt(headerOffset int64) (from, to int64) {
	const batchKeywordAndSpace = int64(len(batchKeyword) + 1)
	from = headerOffset + batchKeywordAndSpace + 20 + 1
	return from, from + 20
}

func writeTo(t *testing.T, dir, name string, raw []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// Scenario: a length field damaged upwards is not an unfinished append
//
//	Given a journal of several confirmed batches
//	And the length in an early batch's header raised past the bytes that follow
//	When the journal is inspected
//	Then it is not reported as an incomplete tail, because the batches after
//	     the damaged one are whole, confirmed, and still on the disk.
//
// This is the claim, isolated from any bit pattern: the reader concludes "the
// file was cut short here" from a length it cannot satisfy, and every
// confirmed batch after that point is inside what it then calls the unfinished
// append. An operator reading the report is told those bytes were never
// confirmed. They were.
func TestALengthRaisedPastTheFileIsNotReportedAsAnUnfinishedAppend(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "journal.praxis")
	writeSessionWithOrder(t, original)
	raw := fileBytes(t, original)

	whole := readJournalBytes(t, raw)
	if len(whole.Batches) < 4 {
		t.Fatalf("the fixture confirms %d batches; this scenario needs at least 4", len(whole.Batches))
	}
	offsets := headerOffsets(t, raw)

	// The second batch, so that what follows it is neither the whole journal
	// nor only its last batch.
	damaged := append([]byte(nil), raw...)
	from, to := lengthFieldAt(offsets[1])
	beyond := uint64(len(raw)) // more than can possibly remain after the header
	copy(damaged[from:to], []byte(fmt.Sprintf("%020d", beyond)))

	report, err := Inspect(writeTo(t, dir, "damaged.praxis", damaged))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	stranded := len(whole.Batches) - report.ConfirmedBatches
	if report.Condition == ConditionIncompleteTail && stranded > 1 {
		t.Errorf("a damaged length field is reported as %q, which Repair discards on Apply alone, "+
			"and %d confirmed batches are inside those %d bytes: %s",
			report.Condition, stranded, report.DiscardedBytes, report.Detail)
	}
}

// Scenario: a repair does not discard a confirmed batch without being told to
//
//	Given that same journal
//	When it is repaired with Apply and without DiscardCorruptBatch
//	Then it does not destroy batches the journal had confirmed.
//
// The consent that is missing is not the consent to repair — Apply is
// explicit. It is the consent to what is discarded, which is the distinction
// RepairOptions exists to draw. The bytes survive in the sidecar, so this is
// not destruction; it is a decision taken on the operator's behalf that
// RepairOptions says is theirs.
func TestARepairDoesNotDiscardConfirmedBatchesOnApplyAlone(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "journal.praxis")
	writeSessionWithOrder(t, original)
	raw := fileBytes(t, original)

	whole := readJournalBytes(t, raw)
	offsets := headerOffsets(t, raw)
	damaged := append([]byte(nil), raw...)
	from, to := lengthFieldAt(offsets[1])
	copy(damaged[from:to], []byte(fmt.Sprintf("%020d", uint64(len(raw)))))
	path := writeTo(t, dir, "damaged.praxis", damaged)

	report, err := Repair(path, RepairOptions{Apply: true})
	if err != nil {
		return // refusing is one of the acceptable answers
	}
	after := readJournalBytes(t, fileBytes(t, path))
	lost := len(whole.Batches) - len(after.Batches)
	if lost > 1 {
		t.Errorf("repair kept %d of %d confirmed batches and discarded %d without consent "+
			"(evidence at %s)", len(after.Batches), len(whole.Batches), lost, report.SidecarPath)
	}
}

// Scenario: every single-bit flip of every length field, classified
//
// The exhaustive sweep, as the regression test. One bit at a time over every
// length field in the journal, and each result sorted into what the operator
// would be told: refused outright, held for consent, or discarded on Apply
// alone. Only the third is a finding, and only when more than the last batch
// is inside it — damage to the last batch's length is indistinguishable from a
// genuinely unfinished append by construction, and no reader can close that.
//
// It is a sweep and not a sample because the window is narrow and the
// boundaries are where it lives: a length raised past MaxPayloadBytes is
// refused, a length raised a little is a checksum failure mid-file, and only a
// length raised into the gap between "past the bytes that remain" and "past
// what a payload may weigh" produces this. A sample would find it on some runs.
func TestEverySingleBitFlipOfALengthFieldIsClassifiedHonestly(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "journal.praxis")
	writeSessionWithOrder(t, original)
	raw := fileBytes(t, original)

	whole := readJournalBytes(t, raw)
	offsets := headerOffsets(t, raw)

	var swept, refused, held, silent int
	var first string
	for n, headerAt := range offsets {
		from, to := lengthFieldAt(headerAt)
		for at := from; at < to; at++ {
			for bit := 0; bit < 8; bit++ {
				flipped := append([]byte(nil), raw...)
				flipped[at] ^= 1 << bit
				swept++

				report, err := Inspect(writeTo(t, dir, "flip.praxis", flipped))
				if err != nil {
					refused++
					continue
				}
				stranded := len(whole.Batches) - report.ConfirmedBatches
				switch {
				case report.Condition == ConditionIncompleteTail && stranded > 1:
					silent++
					if first == "" {
						first = fmt.Sprintf("batch %d, byte %d of the length field, bit %d: "+
							"%d of %d confirmed batches inside %d bytes of \"unfinished append\"",
							n+1, at-from, bit, stranded, len(whole.Batches), report.DiscardedBytes)
					}
				case report.Condition == ConditionClean:
					// A flip that changed nothing readable, or one the reader
					// absorbed; either way nothing is discarded.
				default:
					held++
				}
			}
		}
	}
	t.Logf("%d flips: %d refused, %d held for consent, %d discarded on Apply alone",
		swept, refused, held, silent)
	if silent > 0 {
		t.Errorf("%d of %d single-bit flips of a length field are reported as an unfinished "+
			"append while confirmed batches sit inside the discarded region; first: %s",
			silent, swept, first)
	}
}

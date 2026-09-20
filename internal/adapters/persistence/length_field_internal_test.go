package persistence

import (
	"bytes"
	"errors"
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
//	When the journal is read
//	Then it is refused as damage with data after it, and not reported as an
//	     incomplete tail, because the batches after the damaged one are whole,
//	     confirmed, and still on the disk.
//
// The finding is the one the other door already gives — ErrCorruptMidFile, and
// ConditionFatal out of Inspect — and deliberately not a new name. A batch
// damaged so its checksum fails, with data after it, is refused under that
// error today; this is the same fact reached through the length field, and a
// second name for it would put the client back to discriminating on a label
// that does not match the fact.
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
	copy(damaged[from:to], []byte(fmt.Sprintf("%020d", uint64(len(raw)))))
	path := writeTo(t, dir, "damaged.praxis", damaged)

	if _, err := ReadJournal(bytes.NewReader(damaged)); !errors.Is(err, ErrCorruptMidFile) {
		t.Errorf("the reader answered %v, want %v", err, ErrCorruptMidFile)
	}

	report, err := Inspect(path)
	if !errors.Is(err, ErrNotAJournal) {
		t.Errorf("Inspect answered %v, want it refused as %v", err, ErrNotAJournal)
	}
	if report == nil {
		t.Fatal("Inspect reported nothing at all")
	}
	if report.Condition != ConditionFatal {
		stranded := len(whole.Batches) - report.ConfirmedBatches
		t.Errorf("a damaged length field is reported as %q, and %d of %d confirmed batches "+
			"are inside %d bytes: %s",
			report.Condition, stranded, len(whole.Batches), report.DiscardedBytes, report.Detail)
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

// Scenario: a journal genuinely cut short still repairs on Apply alone
//
//	Given a journal whose last batch was cut in the middle of its payload
//	When it is repaired with Apply and without DiscardCorruptBatch
//	Then it repairs cleanly, because that is what an incomplete tail is.
//
// The guard on the scenarios above. They are satisfied by a reader that calls
// everything fatal, and such a reader would have traded one defect for a worse
// one: repair exists for exactly this case, and a probe that cannot tell it
// from damage takes away the only thing repair is for.
func TestAJournalCutInsideItsLastBatchStillRepairsCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.praxis")
	writeSessionWithOrder(t, path)
	raw := fileBytes(t, path)

	whole := readJournalBytes(t, raw)
	offsets := headerOffsets(t, raw)
	last := offsets[len(offsets)-1]

	// Inside the last batch's payload: past its header line, short of its end.
	_, headerEnd := lengthFieldAt(last)
	cut := int(headerEnd) + 40
	if cut >= len(raw) {
		t.Fatalf("the last batch is too small to cut inside: %d bytes left", len(raw)-int(headerEnd))
	}
	if err := os.WriteFile(path, raw[:cut], 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	report, err := Repair(path, RepairOptions{Apply: true})
	if err != nil {
		t.Fatalf("a genuinely incomplete tail was refused: %v", err)
	}
	if !report.Repaired || report.Condition != ConditionClean {
		t.Fatalf("report: %+v", report)
	}
	if got, want := report.ConfirmedBatches, len(whole.Batches)-1; got != want {
		t.Errorf("the repaired journal confirms %d batches, want %d", got, want)
	}
}

// Scenario: a journal cut short deep inside itself is still an incomplete tail
//
//	Given a journal cut in the middle of its second batch, so that four whole
//	batches are simply not on the disk at all
//	When it is repaired with Apply and without DiscardCorruptBatch
//	Then it repairs cleanly.
//
// This is the case that looks most like the defect and is not it. Both end
// with five of six batches gone; the difference is whether the bytes are
// there. A probe that answered on the count rather than on the bytes would
// refuse this one, and refusing it means a machine killed mid-append cannot be
// recovered — which is the ordinary case, not the exotic one.
func TestAJournalCutDeepInsideItselfStillRepairsCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.praxis")
	writeSessionWithOrder(t, path)
	raw := fileBytes(t, path)

	offsets := headerOffsets(t, raw)
	_, headerEnd := lengthFieldAt(offsets[1])
	cut := int(headerEnd) + 20
	if err := os.WriteFile(path, raw[:cut], 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	report, err := Repair(path, RepairOptions{Apply: true})
	if err != nil {
		t.Fatalf("a journal cut deep inside itself was refused: %v", err)
	}
	if !report.Repaired || report.Condition != ConditionClean {
		t.Fatalf("report: %+v", report)
	}
	if report.ConfirmedBatches != 1 {
		t.Errorf("the repaired journal confirms %d batches, want 1", report.ConfirmedBatches)
	}
}

// headerLineAt is a batch's whole header line, from "BATCH" to the byte before
// its newline.
func headerLineAt(raw []byte, headerOffset int64) (from, to int64) {
	for n := headerOffset; n < int64(len(raw)); n++ {
		if raw[n] == '\n' {
			return headerOffset, n
		}
	}
	return headerOffset, int64(len(raw))
}

// Scenario: every single-bit flip of every header field, classified
//
// The wide sweep, agreed to run after the known mode was closed rather than
// before it: whatever it finds, it finds against a reader that already tells
// damage from an ending, which is a better place to look from.
//
// Two findings are counted, not one. The first is the defect just fixed — an
// unfinished append that confirmed batches are inside. The second is worse and
// was never measured: a journal that reads clean while holding fewer batches
// than it held, which is silent truncation and would be reported to an
// operator as a healthy file.
func TestEverySingleBitFlipOfAHeaderFieldIsClassifiedHonestly(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "journal.praxis")
	writeSessionWithOrder(t, original)
	raw := fileBytes(t, original)

	whole := readJournalBytes(t, raw)
	offsets := headerOffsets(t, raw)

	var swept, refused, held, clean, silent, shortened int
	var first string
	for n, headerAt := range offsets {
		from, to := headerLineAt(raw, headerAt)
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
						first = fmt.Sprintf("batch %d, byte %d of the header, bit %d: "+
							"%d confirmed batches inside an \"unfinished append\"", n+1, at-from, bit, stranded)
					}
				case report.Condition == ConditionClean && stranded > 0:
					shortened++
					if first == "" {
						first = fmt.Sprintf("batch %d, byte %d of the header, bit %d: "+
							"reads clean with %d batches, and the journal had %d",
							n+1, at-from, bit, report.ConfirmedBatches, len(whole.Batches))
					}
				case report.Condition == ConditionClean:
					clean++
				default:
					held++
				}
			}
		}
	}
	t.Logf("%d flips: %d refused, %d held for consent, %d read clean and whole",
		swept, refused, held, clean)
	if silent+shortened > 0 {
		t.Errorf("%d discarded on Apply alone and %d read clean while short, of %d flips; first: %s",
			silent, shortened, swept, first)
	}
}

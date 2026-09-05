package persistence

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"praxis/internal/market"
	"praxis/internal/session"
)

// writeSessionWithOrder builds a journal that contains a decision, not only
// observations. Most fixtures here observe and never submit, which mirrors the
// gap in the system itself: everything durable was built around the flow that
// measures nothing.
func writeSessionWithOrder(t *testing.T, path string) {
	t.Helper()
	s, w := openSessionOnDisk(t, path)
	order, err := market.NewMarketOrder("o-1", mnqInstrument(), market.SideBuy, 2)
	if err != nil {
		t.Fatalf("NewMarketOrder: %v", err)
	}
	if err := s.SubmitOrder(order); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
	if err := s.EndTradingSession(9_000); err != nil {
		t.Fatalf("EndTradingSession: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// writeSession builds a journal on disk with two confirmed commands.
func writeSession(t *testing.T, path string) {
	t.Helper()
	s, w := openSessionOnDisk(t, path)
	if err := s.EndTradingSession(9_000); err != nil {
		t.Fatalf("EndTradingSession: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return b
}

// Scenario: a clean journal is left alone
func TestRepairDoesNothingToACleanJournal(t *testing.T) {
	path := tempJournal(t)
	writeSession(t, path)
	before := fileBytes(t, path)

	report, err := Repair(path, RepairOptions{Apply: true})
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if report.Condition != ConditionClean || report.Repaired {
		t.Fatalf("report: %+v", report)
	}
	if string(fileBytes(t, path)) != string(before) {
		t.Fatal("a clean journal was modified")
	}

	sidecars, _ := filepath.Glob(path + ".tail-*")
	if len(sidecars) != 0 {
		t.Fatalf("a clean repair left evidence files: %v", sidecars)
	}
}

// Scenario: an unfinished append is truncated, and its bytes are kept
//
// Those bytes were never confirmed: the command that produced them returned an
// error and its session stopped. Discarding them loses nothing anyone believed
// had happened, so ordinary consent is enough.
func TestRepairTruncatesAnIncompleteTail(t *testing.T) {
	path := tempJournal(t)
	writeSession(t, path)
	whole := fileBytes(t, path)

	clean, err := Inspect(path)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	confirmed := clean.LastConfirmedOffset

	// Cut inside the last batch.
	cut := int(confirmed) - 10
	if err := os.WriteFile(path, whole[:cut], 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	dry, err := Repair(path, RepairOptions{})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.Condition != ConditionIncompleteTail || dry.Repaired {
		t.Fatalf("dry run: %+v", dry)
	}
	if len(fileBytes(t, path)) != cut {
		t.Fatal("a dry run changed the file")
	}

	report, err := Repair(path, RepairOptions{Apply: true})
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if !report.Repaired || report.Condition != ConditionClean {
		t.Fatalf("report: %+v", report)
	}

	// The evidence is beside the journal, named for where it came from.
	evidence := fileBytes(t, report.SidecarPath)
	if string(evidence) != string(whole[report.LastConfirmedOffset:cut]) {
		t.Fatal("the preserved bytes are not the discarded ones")
	}
	if int64(len(fileBytes(t, path))) != report.LastConfirmedOffset {
		t.Fatal("the journal was not truncated to the last confirmed byte")
	}
}

// Scenario: repairing twice changes nothing the second time
func TestRepairIsIdempotent(t *testing.T) {
	path := tempJournal(t)
	writeSession(t, path)
	whole := fileBytes(t, path)
	if err := os.WriteFile(path, whole[:len(whole)-10], 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	first, err := Repair(path, RepairOptions{Apply: true})
	if err != nil {
		t.Fatalf("first repair: %v", err)
	}
	afterFirst := fileBytes(t, path)
	evidence := fileBytes(t, first.SidecarPath)

	second, err := Repair(path, RepairOptions{Apply: true})
	if err != nil {
		t.Fatalf("second repair: %v", err)
	}
	if second.Repaired {
		t.Fatal("the second repair changed a journal that was already clean")
	}
	if string(fileBytes(t, path)) != string(afterFirst) {
		t.Fatal("repairing twice produced different bytes")
	}
	if string(fileBytes(t, first.SidecarPath)) != string(evidence) {
		t.Fatal("the second repair overwrote the evidence")
	}
}

// Scenario: a batch that was written whole is not discarded without consent
func TestDiscardingAWrittenBatchNeedsItsOwnConsent(t *testing.T) {
	path := tempJournal(t)
	writeSession(t, path)
	whole := fileBytes(t, path)

	clean, err := Inspect(path)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	// Damage a byte inside the last batch's payload, leaving it whole.
	damaged := append([]byte(nil), whole...)
	damaged[clean.LastConfirmedOffset-5] ^= 0xff
	if err := os.WriteFile(path, damaged, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	report, err := Repair(path, RepairOptions{Apply: true})
	if !errors.Is(err, ErrCorruptBatchHeld) {
		t.Fatalf("error: got %v, want %v", err, ErrCorruptBatchHeld)
	}
	if report.Condition != ConditionCorruptLastBatch {
		t.Fatalf("condition: got %v", report.Condition)
	}
	if report.CorruptBatch == nil || report.CorruptBatch.Number == 0 {
		t.Fatalf("the report does not name the batch: %+v", report)
	}
	if string(fileBytes(t, path)) != string(damaged) {
		t.Fatal("the journal was modified without consent")
	}

	report, err = Repair(path, RepairOptions{Apply: true, DiscardCorruptBatch: true})
	if err != nil {
		t.Fatalf("with consent: %v", err)
	}
	if !report.Repaired {
		t.Fatalf("report: %+v", report)
	}
}

// Scenario: damage inside a journal is never repaired by truncation
func TestRepairRefusesDamageInsideAJournal(t *testing.T) {
	path := tempJournal(t)
	writeSession(t, path)
	whole := fileBytes(t, path)

	// Damage the first batch, which has batches after it.
	at := 0
	for n := 0; n+15 < len(whole); n++ {
		if string(whole[n:n+15]) == "session_started" {
			at = n
			break
		}
	}
	if at == 0 {
		t.Fatal("the first payload was not found")
	}
	whole[at+16] ^= 0xff
	if err := os.WriteFile(path, whole, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	before := fileBytes(t, path)
	if _, err := Repair(path, RepairOptions{Apply: true, DiscardCorruptBatch: true}); !errors.Is(err, ErrNotAJournal) {
		t.Fatalf("error: got %v, want it refused", err)
	}
	if string(fileBytes(t, path)) != string(before) {
		t.Fatal("a journal damaged inside was modified")
	}
}

// Scenario: a journal with nothing confirmed repairs to a valid empty journal
func TestAJournalWithNothingConfirmed(t *testing.T) {
	path := tempJournal(t)
	w, err := OpenWriter(path, DurableEveryBatch)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	if _, err := w.Append(twoEvents()); err != nil {
		t.Fatalf("Append: %v", err)
	}
	w.Close()

	whole := fileBytes(t, path)
	if err := os.WriteFile(path, whole[:len(whole)-5], 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	report, err := Repair(path, RepairOptions{Apply: true})
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if !report.Repaired || report.ConfirmedBatches != 0 {
		t.Fatalf("report: %+v", report)
	}
	if string(fileBytes(t, path)) != string(Header()) {
		t.Fatal("the repaired journal is not just a valid header")
	}
	// And a writer can carry on from it.
	again, err := OpenWriter(path, DurableEveryBatch)
	if err != nil {
		t.Fatalf("a repaired empty journal will not open: %v", err)
	}
	again.Close()
}

// Scenario: evidence is never replaced
func TestEvidenceIsNeverReplaced(t *testing.T) {
	path := tempJournal(t)
	writeSession(t, path)
	whole := fileBytes(t, path)
	if err := os.WriteFile(path, whole[:len(whole)-10], 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	clean, err := Inspect(path)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	discarded := whole[clean.LastConfirmedOffset : len(whole)-10]
	name := preservedName(path, clean.LastConfirmedOffset, discarded)
	if err := os.WriteFile(name, []byte("something else entirely"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	before := fileBytes(t, path)
	if _, err := Repair(path, RepairOptions{Apply: true}); !errors.Is(err, ErrEvidenceDiffers) {
		t.Fatalf("error: got %v, want %v", err, ErrEvidenceDiffers)
	}
	if string(fileBytes(t, path)) != string(before) {
		t.Fatal("the journal was truncated although the evidence could not be kept")
	}
}

// Scenario: neither command touches a journal a writer holds
func TestNeitherCommandTouchesAJournalAWriterHolds(t *testing.T) {
	path := tempJournal(t)
	w, err := OpenWriter(path, DurableEveryBatch)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	defer w.Close()

	if _, err := Inspect(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("Inspect: got %v, want %v", err, ErrLocked)
	}
	if _, err := Repair(path, RepairOptions{}); !errors.Is(err, ErrLocked) {
		t.Fatalf("Repair dry run: got %v, want %v", err, ErrLocked)
	}
}

func TestInspectRefusesWhatIsNotAJournal(t *testing.T) {
	dir := t.TempDir()

	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := Inspect(empty); !errors.Is(err, ErrNotAJournal) {
		t.Fatalf("an empty file: got %v, want %v", err, ErrNotAJournal)
	}

	if _, err := Inspect(filepath.Join(dir, "absent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing file: got %v, want it not to exist", err)
	}
}

// A session actually recovers from a repaired journal.
func TestARepairedJournalStillDrivesASession(t *testing.T) {
	path := tempJournal(t)
	writeSession(t, path)
	whole := fileBytes(t, path)
	if err := os.WriteFile(path, whole[:len(whole)-8], 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := Repair(path, RepairOptions{Apply: true}); err != nil {
		t.Fatalf("Repair: %v", err)
	}

	state, _, err := Recover(path)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	resumed, err := session.Resume(state, nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := resumed.Observe(market.Quote{
		Instrument: mnqInstrument(), Time: 20_000, Bid: 20_000, Ask: 20_001, BidSize: 1, AskSize: 1,
	}, 1); err != nil && !errors.Is(err, session.ErrNoSessionOpen) {
		t.Fatalf("the recovered session is unusable: %v", err)
	}
}

// Scenario: inspection shares, repair excludes
//
//	Given a journal already held under a shared lock
//	When it is inspected, it succeeds; when a repair is attempted, it does
//	  not.
//
// Two readers may look at a journal at once, and a repair may not run while
// anyone is looking. The two locks are held on separate open file
// descriptions, which is what makes them conflict; two locks taken on the same
// descriptor would not be a fair test of anything.
func TestInspectionSharesWhereRepairExcludes(t *testing.T) {
	path := tempJournal(t)
	writeSession(t, path)

	reader, err := openLocked(path, false)
	if err != nil {
		t.Fatalf("taking a shared lock: %v", err)
	}
	defer reader.Close()

	if _, err := Inspect(path); err != nil {
		t.Fatalf("a second reader was refused: %v", err)
	}
	if _, err := Repair(path, RepairOptions{}); !errors.Is(err, ErrLocked) {
		t.Fatalf("repair ran while a reader held the journal: %v", err)
	}
}

// forge rewrites a journal with one fact altered, re-framed so that every
// checksum matches. The result is byte-perfect at the frame level and false at
// the domain level, which is precisely the case inspection cannot see.
func forge(t *testing.T, path string, alter func(session.Event) (session.Event, bool)) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	journal, err := ReadJournal(f)
	f.Close()
	if err != nil {
		t.Fatalf("ReadJournal: %v", err)
	}

	out := Header()
	altered := false
	for _, b := range journal.Batches {
		events := make([]session.Event, len(b.Events))
		copy(events, b.Events)
		for n, e := range events {
			if got, ok := alter(e); ok && !altered {
				events[n], altered = got, true
			}
		}
		framed, err := EncodeBatch(b.Number, events, EventVersion)
		if err != nil {
			t.Fatalf("EncodeBatch: %v", err)
		}
		out = append(out, framed...)
	}
	if !altered {
		t.Fatal("nothing was altered")
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// Scenario: a forged journal passes inspection and fails proof
//
//	Given a journal in which one recorded valuation was altered and every
//	  checksum recomputed
//	When it is inspected, it is clean — the bytes are exactly the bytes that
//	  were written
//	And when it is proved, it is refused, because no account could have
//	  produced that valuation.
//
// Checksums answer "were these bytes damaged". They cannot answer "could this
// have happened", and an operator reading "clean" will believe the second.
func TestAForgedJournalInspectsCleanAndFailsProof(t *testing.T) {
	path := tempJournal(t)
	writeSession(t, path)

	if report, err := Prove(path); err != nil || !report.Proved {
		t.Fatalf("the honest journal does not prove: %v %+v", err, report)
	}

	forge(t, path, func(e session.Event) (session.Event, bool) {
		v, ok := e.(session.AccountValued)
		if !ok {
			return nil, false
		}
		v.EquityCts -= 1
		return v, true
	})

	report, err := Inspect(path)
	if err != nil || report.Condition != ConditionClean {
		t.Fatalf("inspection: got %v %v, want it to see nothing wrong", report.Condition, err)
	}

	report, err = Prove(path)
	if !errors.Is(err, ErrNotProvable) {
		t.Fatalf("proof: got %v, want %v", err, ErrNotProvable)
	}
	if report.Proved {
		t.Fatal("a forged journal was reported as proved")
	}
}

// The same, for a fact the log can be made self-consistent about: Verify
// catches this one and Replay would too.
func TestAForgedContextIsCaughtByProof(t *testing.T) {
	path := tempJournal(t)
	writeSessionWithOrder(t, path)

	forge(t, path, func(e session.Event) (session.Event, bool) {
		o, ok := e.(session.OrderSubmitted)
		if !ok {
			return nil, false
		}
		o.Context.ConsecutiveLosses++
		return o, true
	})

	if report, err := Inspect(path); err != nil || report.Condition != ConditionClean {
		t.Fatalf("inspection saw a problem it cannot see: %v %v", report.Condition, err)
	}
	if _, err := Prove(path); !errors.Is(err, ErrNotProvable) {
		t.Fatalf("proof: got %v, want %v", err, ErrNotProvable)
	}
}

// Proof takes a shared lock, like inspection, and refuses a journal a writer
// holds.
func TestProofSharesAndRefusesABusyJournal(t *testing.T) {
	path := tempJournal(t)
	writeSession(t, path)

	reader, err := openLocked(path, false)
	if err != nil {
		t.Fatalf("shared lock: %v", err)
	}
	if _, err := Prove(path); err != nil {
		t.Fatalf("a second reader was refused: %v", err)
	}
	reader.Close()

	w, err := OpenWriter(path, DurableEveryBatch)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	defer w.Close()
	if _, err := Prove(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("proof ran while a writer held the journal: %v", err)
	}
}

// Scenario: a journal written in the older schema keeps working
//
//	Given a journal whose header says v1
//	When it is read, proved and appended to
//	Then all three succeed, and the appended batches are still v1 — a
//	  journal is not rewritten into a newer schema by being written to.
//
// This is what the compatibility table was built for, exercised now rather than
// once there is data in it worth losing.
func TestAV1JournalIsStillReadableAndAppendable(t *testing.T) {
	path := tempJournal(t)

	// Written deliberately in the older schema.
	events := []session.Event{SessionStartedFixture(1)}
	framed, err := EncodeBatch(1, events, EventVersionV1)
	if err != nil {
		t.Fatalf("EncodeBatch: %v", err)
	}
	if err := os.WriteFile(path, append(HeaderFor(EventVersionV1), framed...), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	journal, err := func() (*Journal, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return ReadJournal(f)
	}()
	if err != nil {
		t.Fatalf("ReadJournal: %v", err)
	}
	if journal.PayloadVersion != EventVersionV1 {
		t.Fatalf("version: got %q, want v1", journal.PayloadVersion)
	}

	w, err := OpenWriter(path, DurableEveryBatch)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	if w.PayloadVersion() != EventVersionV1 {
		t.Fatalf("the writer would upgrade the journal to %q", w.PayloadVersion())
	}
	if _, err := w.Append([]session.Event{
		session.SessionOpened{
			Envelope:  session.Envelope{Time: 2_000, Sequence: 2, Kind: session.KindSessionOpened},
			SessionID: "d1", BalanceCts: 5_000_000, EquityCts: 5_000_000,
		},
	}); err != nil {
		t.Fatalf("Append to a v1 journal: %v", err)
	}
	w.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.HasPrefix(string(raw), "PRAXIS-EVENT-STORE 1 "+EventVersionV1) {
		t.Fatal("appending changed the journal's schema")
	}
}

// A v1 journal cannot be given a fact its schema has no room for.
func TestAV1JournalRefusesAV2Fact(t *testing.T) {
	path := tempJournal(t)
	framed, err := EncodeBatch(1, []session.Event{SessionStartedFixture(1)}, EventVersionV1)
	if err != nil {
		t.Fatalf("EncodeBatch: %v", err)
	}
	if err := os.WriteFile(path, append(HeaderFor(EventVersionV1), framed...), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	w, err := OpenWriter(path, DurableEveryBatch)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	defer w.Close()

	_, err = w.Append([]session.Event{
		session.ProtectionPlaced{
			Envelope:     session.Envelope{Time: 2_000, Sequence: 2, Kind: session.KindProtectionPlaced},
			EntryOrderID: "o-1", StopPrice: 19_900,
		},
	})
	if !errors.Is(err, ErrUnsupportedInVersion) {
		t.Fatalf("error: got %v, want %v", err, ErrUnsupportedInVersion)
	}
}

// Command praxis operates a Praxis journal.
//
// This is the real command, not a scaffold to be replaced. The replay loop
// will join it as a sibling subcommand rather than as a second binary.
//
//	praxis ui <market-file> --journal <journal> [configuration]
//	praxis store inspect <journal>
//	praxis store verify  <journal> [<market-file>] [--anchor <reference>]
//	praxis store anchor  <journal>
//	praxis store repair  <journal> [--apply] [--discard-corrupt-batch]
//
// Exit codes are meant to be automated against:
//
//	0  clean, or repaired and validated
//	1  repairable, and nothing was changed
//	2  fatal: exists but is not a journal, is damaged beyond truncation, or
//	   the anchor reference supplied could not be read
//	3  busy: another process holds the journal
//	4  no such input
//	5  frames intact, history not provable
//	6  history provable, and this is not the journal the anchor confirms
//	7  history provable, and the journal does not describe the market file
//	   supplied with it
//
// 5, 6 and 7 are separate because they are separate findings, and this list is
// the surface a script reads. A journal cut at a batch boundary is provable —
// its history could have happened — and is missing acts that were confirmed. A
// journal checked against a market file whose rows are not the ones it recorded
// can be provable and complete, and its decisions were not taken against the
// prices that file shows. One code for
// any two of them would collapse a distinction ADR-015 exists to draw, on the
// only surface where nobody can read the sentences that draw it.
//
// Several can hold at once. The code then names the most fundamental, in the
// order 5, 7, 6: a history that could not have happened; then a record that does
// not match the prices supplied with it, because that changes what every decision
// meant; then a journal missing acts, which leaves the remaining decisions'
// meaning intact.
// The order is written here rather than left to the order of the checks, the
// way challenge.Observe fixes the order of its own rules. The output names every
// finding that holds, because the code carries one and the report carries what
// an operator has to act on.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"praxis/internal/adapters/marketdata"
	"praxis/internal/adapters/persistence"
	"praxis/internal/session"
)

const (
	exitClean       = 0
	exitRepairable  = 1
	exitFatal       = 2
	exitBusy        = 3
	exitNoInput     = 4
	exitNotProvable = 5

	// exitAnchorMismatch is provable and incomplete, which is not the same
	// finding as unprovable and must not share its code.
	exitAnchorMismatch = 6

	// exitStimulusMismatch is a journal whose observations are not the rows of the
	// market file supplied with it. It outranks a cut journal and is outranked by
	// an impossible one.
	exitStimulusMismatch = 7
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, out, errOut *os.File) int {
	if len(args) == 0 {
		usage(errOut)
		return exitFatal
	}
	switch args[0] {
	case "store":
		return storeCommand(args[1:], out, errOut)
	case "replay":
		return replayCommand(args[1:], out, errOut)
	case "ui":
		return uiCommand(args[1:], out, errOut)
	default:
		fmt.Fprintf(errOut, "praxis: unknown command %q\n", args[0])
		usage(errOut)
		return exitFatal
	}
}

func storeCommand(args []string, out, errOut *os.File) int {
	if len(args) == 0 {
		usage(errOut)
		return exitFatal
	}
	switch args[0] {
	case "inspect":
		return inspect(args[1:], out, errOut)
	case "verify":
		return verify(args[1:], out, errOut)
	case "anchor":
		return anchor(args[1:], out, errOut)
	case "repair":
		return repair(args[1:], out, errOut)
	default:
		fmt.Fprintf(errOut, "praxis store: unknown subcommand %q\n", args[0])
		usage(errOut)
		return exitFatal
	}
}

func inspect(args []string, out, errOut *os.File) int {
	fs := flag.NewFlagSet("praxis store inspect", flag.ContinueOnError)
	fs.SetOutput(errOut)
	flags, operands := splitArgs(fs, args)
	if err := fs.Parse(flags); err != nil || len(operands) != 1 {
		fmt.Fprintln(errOut, "usage: praxis store inspect <journal>")
		return exitFatal
	}

	report, err := persistence.Inspect(operands[0])
	if code, handled := classify(err, errOut); handled {
		return code
	}
	printReport(out, report)
	if report.Condition == persistence.ConditionClean {
		return exitClean
	}
	return exitRepairable
}

// anchor emits the reference a journal is later checked against.
//
// It only writes the value down. Where it is then kept is the whole question —
// an anchor left beside the journal it anchors is well formed and worthless,
// because whoever can cut the journal can rewrite the file next to it. This
// command prints to standard output for exactly that reason: what happens to
// the line is a custody decision and not this program's to make.
//
// It takes no market file, and needs none. The journal's observations are inside
// the prefix it digests, and Consumed compares every row of a market file with
// them field by field, so anchoring the journal already binds the content of
// every row it consumed. A second digest over the file's bytes would add only
// the bytes that do not change what the rows mean — see ADR-015.
func anchor(args []string, out, errOut *os.File) int {
	fs := flag.NewFlagSet("praxis store anchor", flag.ContinueOnError)
	fs.SetOutput(errOut)
	flags, operands := splitArgs(fs, args)
	if err := fs.Parse(flags); err != nil || len(operands) != 1 {
		fmt.Fprintln(errOut, "usage: praxis store anchor <journal>")
		return exitFatal
	}

	taken, err := persistence.AnchorOf(operands[0])
	if code, handled := classify(err, errOut); handled {
		return code
	}
	text, err := taken.Format()
	if err != nil {
		fmt.Fprintf(errOut, "praxis: %v\n", err)
		return exitFatal
	}
	fmt.Fprintln(out, text)
	return exitClean
}

// verify is inspect plus the proof. They are separate commands because they
// make different claims and an operator must be able to tell which one they
// asked for: inspection says the bytes are intact, and only this says the
// history in them could have happened.
func verify(args []string, out, errOut *os.File) int {
	fs := flag.NewFlagSet("praxis store verify", flag.ContinueOnError)
	fs.SetOutput(errOut)
	reference := fs.String("anchor", "", "the anchor this journal is checked against, as praxis store anchor emits it")
	flags, operands := splitArgs(fs, args)
	if err := fs.Parse(flags); err != nil || len(operands) < 1 || len(operands) > 2 {
		fmt.Fprintln(errOut, "usage: praxis store verify <journal> [<market-file>] [--anchor <reference>]")
		return exitFatal
	}

	// Parsed before the journal is opened: a reference that cannot be read is
	// the reference's fault, and reading the file to say so would be reporting
	// on a journal about which nothing was asked.
	var want *persistence.Anchor
	if *reference != "" {
		parsed, err := persistence.ParseAnchor(*reference)
		if err != nil {
			fmt.Fprintln(out, "anchored:  not checked — the reference could not be read")
			fmt.Fprintf(errOut, "praxis: %v\n", err)
			return exitFatal
		}
		want = &parsed
	}

	var feed *marketdata.Feed
	if len(operands) == 2 {
		loaded, err := marketdata.ReadFile(operands[1])
		if code, handled := classify(err, errOut); handled {
			fmt.Fprintln(out, "stimulus:  not checked — the market file could not be read")
			return code
		}
		feed = loaded
	}

	report, err := persistence.ProveAgainst(operands[0], want)
	if errors.Is(err, persistence.ErrNotProvable) {
		printReport(out, report)
		// The completeness claim was computed before Verify and Replay so that
		// it would survive this return. Printing it here is what makes that
		// true: without it, passing --anchor to an unprovable journal produced
		// output byte-identical to passing nothing, and an operator could not
		// tell whether the second claim had even been evaluated.
		anchoredLine(out, report.Anchor)
		// The stimulus claim is made here too, for the reason the anchor claim
		// is: this return must not be where a second finding disappears.
		stimulus := stimulusOf(report.Events, feed, report.Anchor)
		stimulusLine(out, stimulus)
		fmt.Fprintf(errOut, "\npraxis: %v\n", err)
		fmt.Fprintln(errOut, "the frames are intact: every batch's checksum matches its bytes.")
		fmt.Fprintln(errOut, "what does not hold is the history — some fact in this journal could")
		fmt.Fprintln(errOut, "not have been produced by the account and evaluation it describes.")
		anchorFailure(errOut, report.Anchor, false)
		stimulusFailure(errOut, stimulus)
		return exitNotProvable
	}
	if code, handled := classify(err, errOut); handled {
		return code
	}

	printReport(out, report)
	fmt.Fprintf(out, "proved:    %d events replayed against the aggregates that produced them\n", report.EventsProved)
	// The second claim, and never folded into the first. What "proved" does not
	// cover is said even when nothing was supplied to check it against: a
	// journal cut at a batch boundary is a shorter journal and verifies exactly
	// like this one, so an operator reading the line above alone would take
	// coherence for completeness — see ADR-015.
	anchoredLine(out, report.Anchor)
	stimulus := stimulusOf(report.Events, feed, report.Anchor)
	stimulusLine(out, stimulus)

	// 7 before 6, as the header says. Both explanations are printed when both
	// hold; only the order of the codes is decided.
	anchorFailed := report.Anchor != nil && report.Anchor.Err != nil
	if anchorFailed {
		anchorFailure(errOut, report.Anchor, true)
	}
	if stimulus.failed() {
		stimulusFailure(errOut, stimulus)
		return exitStimulusMismatch
	}
	if anchorFailed {
		return exitAnchorMismatch
	}
	if report.Condition != persistence.ConditionClean {
		fmt.Fprintln(out, "\nthe confirmed part of this journal is provable; its tail is not confirmed.")
		return exitRepairable
	}
	return exitClean
}

// anchoredLine says which completeness claim was made, in words.
//
// It opens nothing: the claim was made under the same lock as the coherence one
// above it, so the two lines describe one file. Each outcome is named rather
// than distinguished by the case of two letters — three states separated by
// "no" against "NO" is too little margin for a report whose whole subject is
// that one claim must not read as another.
func anchoredLine(out io.Writer, claim *persistence.AnchorClaim) {
	switch {
	case claim == nil:
		fmt.Fprintln(out, "anchored:  not checked — no reference was supplied, so completeness")
		fmt.Fprintln(out, "           was not evaluated; a journal with batches removed at a")
		fmt.Fprintln(out, "           boundary would verify exactly like this one")
	case claim.Err != nil:
		fmt.Fprintln(out, "anchored:  FAILED — this is not the journal the anchor confirms")
	default:
		fmt.Fprintf(out, "anchored:  yes — through batch %d, sequence %d, and the bytes leading to it\n",
			claim.Want.LastBatch, claim.Want.LastSequence)
		fmt.Fprintln(out, "           nothing bounds what came after it")
	}
}

// anchorFailure explains a completeness claim that failed.
//
// proved is what the other claim found, and it changes what is true to say. On
// a journal that proves, the useful sentence is that coherence is a different
// claim; on one that does not, that sentence would be false, and the useful one
// is that both failed and which of them the exit code names.
func anchorFailure(errOut io.Writer, claim *persistence.AnchorClaim, proved bool) {
	if claim == nil || claim.Err == nil {
		return
	}
	fmt.Fprintf(errOut, "\npraxis: %v\n", claim.Err)
	if proved {
		fmt.Fprintln(errOut, "the history above is coherent, and that is a different claim from this")
		fmt.Fprintln(errOut, "one. a journal can be internally consistent and still be missing acts")
		fmt.Fprintln(errOut, "that were confirmed, which is exactly what an anchor is for.")
		return
	}
	fmt.Fprintln(errOut, "this journal fails both claims: its history could not have happened, and")
	fmt.Fprintln(errOut, "it is not the journal the anchor confirms. the exit code names the first,")
	fmt.Fprintln(errOut, "which is the more fundamental of the two.")
}

// stimulusClaim is what checking a journal against a market file found: whether
// the journal's observations are this file's rows.
//
// There is no second digest of the file to compare, and there does not need to
// be. When the journal's anchor holds, the observations in its anchored prefix
// are fixed from outside, and Consumed compares every row with them field by
// field — so the content of every row that prefix consumed is fixed too, and a
// substitution made consistently in both files is caught as a journal that is
// not the one anchored. What no check here examines is the file's spelling:
// line endings and quoting that change the bytes and not the rows.
type stimulusClaim struct {
	rows      int
	describes error

	// anchorHeld is whether the journal's anchor was supplied and held, which
	// decides what correspondence is worth: bound through the anchor, or a
	// statement about two files one hand could have written together.
	anchorHeld bool
}

// stimulusOf makes the claim over the events verify already proved, so the
// journal it speaks about is the one the lines above it speak about. No market
// file means no claim, and the line says so.
func stimulusOf(events []session.Event, feed *marketdata.Feed, anchored *persistence.AnchorClaim) *stimulusClaim {
	if feed == nil {
		return nil
	}
	claim := &stimulusClaim{anchorHeld: anchored != nil && anchored.Err == nil}
	claim.rows, claim.describes = marketdata.Consumed(events, feed)
	return claim
}

func (c *stimulusClaim) failed() bool { return c != nil && c.describes != nil }

// stimulusLine renders the claim, and qualifies a correspondence by what binds it
// rather than letting "corresponds" be read as "authentic".
func stimulusLine(out io.Writer, c *stimulusClaim) {
	switch {
	case c == nil:
		fmt.Fprintln(out, "stimulus:  not checked — no market file was supplied")
	case c.describes != nil:
		fmt.Fprintln(out, "stimulus:  FAILED — the journal does not describe the supplied market file")
	case c.anchorHeld:
		fmt.Fprintf(out, "stimulus:  corresponds — the journal describes the supplied file through row %d\n", c.rows)
		fmt.Fprintln(out, "           the rows its anchored prefix consumed are bound through the anchor;")
		fmt.Fprintln(out, "           rows consumed after it are not")
	default:
		fmt.Fprintf(out, "stimulus:  corresponds — the journal describes the supplied file through row %d\n", c.rows)
		fmt.Fprintln(out, "           no anchor held, so nothing outside these two files binds them: a")
		fmt.Fprintln(out, "           substitution made consistently in both would pass")
	}
}

// stimulusFailure explains a stimulus claim that failed.
func stimulusFailure(errOut io.Writer, c *stimulusClaim) {
	if !c.failed() {
		return
	}
	fmt.Fprintf(errOut, "\npraxis: %v\n", c.describes)
	fmt.Fprintln(errOut, "the journal records the prices each decision was taken against, and they")
	fmt.Fprintln(errOut, "are not this file's. either the file is not the one the run was produced")
	fmt.Fprintln(errOut, "from, or the journal's record of it was changed.")
}

func repair(args []string, out, errOut *os.File) int {
	fs := flag.NewFlagSet("praxis store repair", flag.ContinueOnError)
	fs.SetOutput(errOut)
	apply := fs.Bool("apply", false, "perform the repair; without it this is a dry run")
	discard := fs.Bool("discard-corrupt-batch", false,
		"consent to discarding a batch that was fully written and may have been confirmed")
	flags, operands := splitArgs(fs, args)
	if err := fs.Parse(flags); err != nil || len(operands) != 1 {
		fmt.Fprintln(errOut, "usage: praxis store repair <journal> [--apply] [--discard-corrupt-batch]")
		return exitFatal
	}

	report, err := persistence.Repair(operands[0], persistence.RepairOptions{
		Apply: *apply, DiscardCorruptBatch: *discard,
	})
	if errors.Is(err, persistence.ErrCorruptBatchHeld) {
		printReport(out, report)
		fmt.Fprintf(errOut, "\npraxis: %v\n", err)
		fmt.Fprintln(errOut, "these bytes were written whole, so the command may have been reported as confirmed.")
		fmt.Fprintln(errOut, "pass --discard-corrupt-batch as well if you accept losing it.")
		return exitRepairable
	}
	if code, handled := classify(err, errOut); handled {
		return code
	}

	printReport(out, report)
	switch {
	case report.Repaired:
		fmt.Fprintf(out, "repaired: truncated to %d bytes, evidence kept at %s\n",
			report.LastConfirmedOffset, report.SidecarPath)
		return exitClean
	case report.Condition == persistence.ConditionClean:
		return exitClean
	default:
		fmt.Fprintln(out, "\nnothing was changed. re-run with --apply to repair.")
		return exitRepairable
	}
}

// splitArgs separates flags from operands so that a journal path may come
// before its flags. Go's flag package stops at the first operand, which would
// make "repair journal --apply" silently a dry run — the one misreading these
// commands must not have.
//
// It asks the flag set whether each flag takes a value, rather than assuming.
// An earlier version assumed every flag was a boolean, which was true of the
// store commands and false the moment replay arrived: "--commission 50" lost
// its value and 50 became a second path.
func splitArgs(fs *flag.FlagSet, args []string) (flags, operands []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if len(arg) < 2 || arg[0] != '-' {
			operands = append(operands, arg)
			continue
		}
		flags = append(flags, arg)
		if strings.Contains(arg, "=") {
			continue
		}
		defined := fs.Lookup(strings.TrimLeft(arg, "-"))
		if defined == nil {
			continue
		}
		if b, ok := defined.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return flags, operands
}

// classify turns an error into an exit code, or reports that it did not.
func classify(err error, errOut *os.File) (int, bool) {
	switch {
	case err == nil:
		return 0, false
	case errors.Is(err, os.ErrNotExist):
		fmt.Fprintf(errOut, "praxis: %v\n", err)
		return exitNoInput, true
	case errors.Is(err, persistence.ErrLocked):
		fmt.Fprintln(errOut, "praxis: another process holds this journal.")
		fmt.Fprintln(errOut, "confirmed batches could be read safely, but the tail is what is moving,")
		fmt.Fprintln(errOut, "so any report about it would describe something mid-flight.")
		return exitBusy, true
	default:
		fmt.Fprintf(errOut, "praxis: %v\n", err)
		return exitFatal, true
	}
}

func printReport(out *os.File, r *persistence.Report) {
	if r == nil {
		return
	}
	fmt.Fprintf(out, "journal:   %s\n", r.Path)
	fmt.Fprintf(out, "versions:  container %s, payload %s\n", r.ContainerVersion, r.PayloadVersion)
	fmt.Fprintf(out, "batches:   %d confirmed\n", r.ConfirmedBatches)
	fmt.Fprintf(out, "sequence:  last confirmed event %d\n", r.LastSequence)
	fmt.Fprintf(out, "offset:    last confirmed byte %d\n", r.LastConfirmedOffset)
	fmt.Fprintf(out, "tail:      %s\n", r.Tail)
	fmt.Fprintf(out, "condition: %s\n", r.Condition)
	if r.DiscardedBytes > 0 {
		fmt.Fprintf(out, "at risk:   %d bytes\n", r.DiscardedBytes)
	}
	if r.CorruptBatch != nil {
		fmt.Fprintf(out, "batch:     number %d, events %d..%d, %d events\n",
			r.CorruptBatch.Number, r.CorruptBatch.FirstSequence,
			r.CorruptBatch.LastSequence, r.CorruptBatch.EventCount)
	}
	if r.ConfigDigest != "" {
		// Everything a journal proves, it proves relative to its
		// configuration, and nothing inside can check the configuration
		// itself. This is what a pre-registration records beforehand, so the
		// axiom can be confirmed from outside.
		fmt.Fprintf(out, "config:    sha256:%s\n", r.ConfigDigest)
		who := r.SubjectID
		if who == "" {
			who = "nobody recorded"
		}
		fmt.Fprintf(out, "subject:   %s\n", who)
	}
	if r.Detail != "" {
		fmt.Fprintf(out, "detail:    %s\n", r.Detail)
	}
}

func usage(out *os.File) {
	fmt.Fprintln(out, "usage:")
	fmt.Fprintln(out, "  praxis replay <market-file> --journal <journal> [configuration]")
	fmt.Fprintln(out, "  praxis ui     <market-file> --journal <journal> --subject <label>")
	fmt.Fprintln(out, "  praxis store inspect <journal>")
	fmt.Fprintln(out, "  praxis store verify  <journal> [<market-file>] [--anchor <reference>]")
	fmt.Fprintln(out, "  praxis store anchor  <journal>")
	fmt.Fprintln(out, "  praxis store repair  <journal> [--apply] [--discard-corrupt-batch]")
}

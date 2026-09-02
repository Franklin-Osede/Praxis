// Command praxis operates a Praxis journal.
//
// This is the real command, not a scaffold to be replaced. The replay loop
// will join it as a sibling subcommand rather than as a second binary.
//
//	praxis store inspect <journal>
//	praxis store verify  <journal>
//	praxis store repair  <journal> [--apply] [--discard-corrupt-batch]
//
// Exit codes are meant to be automated against:
//
//	0  clean, or repaired and validated
//	1  repairable, and nothing was changed
//	2  fatal: exists but is not a journal, or is damaged beyond truncation
//	3  busy: another process holds the journal
//	4  no such input
//	5  frames intact, history not provable
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"praxis/internal/adapters/persistence"
)

const (
	exitClean       = 0
	exitRepairable  = 1
	exitFatal       = 2
	exitBusy        = 3
	exitNoInput     = 4
	exitNotProvable = 5
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

// verify is inspect plus the proof. They are separate commands because they
// make different claims and an operator must be able to tell which one they
// asked for: inspection says the bytes are intact, and only this says the
// history in them could have happened.
func verify(args []string, out, errOut *os.File) int {
	fs := flag.NewFlagSet("praxis store verify", flag.ContinueOnError)
	fs.SetOutput(errOut)
	flags, operands := splitArgs(fs, args)
	if err := fs.Parse(flags); err != nil || len(operands) != 1 {
		fmt.Fprintln(errOut, "usage: praxis store verify <journal>")
		return exitFatal
	}

	report, err := persistence.Prove(operands[0])
	if errors.Is(err, persistence.ErrNotProvable) {
		printReport(out, report)
		fmt.Fprintf(errOut, "\npraxis: %v\n", err)
		fmt.Fprintln(errOut, "the frames are intact: every batch's checksum matches its bytes.")
		fmt.Fprintln(errOut, "what does not hold is the history — some fact in this journal could")
		fmt.Fprintln(errOut, "not have been produced by the account and evaluation it describes.")
		return exitNotProvable
	}
	if code, handled := classify(err, errOut); handled {
		return code
	}

	printReport(out, report)
	fmt.Fprintf(out, "proved:    %d events replayed against the aggregates that produced them\n", report.EventsProved)
	if report.Condition != persistence.ConditionClean {
		fmt.Fprintln(out, "\nthe confirmed part of this journal is provable; its tail is not confirmed.")
		return exitRepairable
	}
	return exitClean
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
	if r.Detail != "" {
		fmt.Fprintf(out, "detail:    %s\n", r.Detail)
	}
}

func usage(out *os.File) {
	fmt.Fprintln(out, "usage:")
	fmt.Fprintln(out, "  praxis replay <market-file> --journal <journal> [configuration]")
	fmt.Fprintln(out, "  praxis store inspect <journal>")
	fmt.Fprintln(out, "  praxis store verify  <journal>")
	fmt.Fprintln(out, "  praxis store repair  <journal> [--apply] [--discard-corrupt-batch]")
}

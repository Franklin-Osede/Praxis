// Command praxis operates a Praxis journal.
//
// This is the real command, not a scaffold to be replaced. The replay loop
// will join it as a sibling subcommand rather than as a second binary.
//
//	praxis store inspect <journal>
//	praxis store repair  <journal> [--apply] [--discard-corrupt-batch]
//
// Exit codes are meant to be automated against:
//
//	0  clean, or repaired and validated
//	1  repairable, and nothing was changed
//	2  fatal: exists but is not a journal, or is damaged beyond truncation
//	3  busy: another process holds the journal
//	4  no such input
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"praxis/internal/adapters/persistence"
)

const (
	exitClean      = 0
	exitRepairable = 1
	exitFatal      = 2
	exitBusy       = 3
	exitNoInput    = 4
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
	flags, operands := splitArgs(args)
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

func repair(args []string, out, errOut *os.File) int {
	fs := flag.NewFlagSet("praxis store repair", flag.ContinueOnError)
	fs.SetOutput(errOut)
	apply := fs.Bool("apply", false, "perform the repair; without it this is a dry run")
	discard := fs.Bool("discard-corrupt-batch", false,
		"consent to discarding a batch that was fully written and may have been confirmed")
	flags, operands := splitArgs(args)
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
// make "repair journal --apply" silently a dry run — the one misreading this
// command must not have. Every flag here is a boolean, so no value can be
// mistaken for a path.
func splitArgs(args []string) (flags, operands []string) {
	for _, a := range args {
		if len(a) > 1 && a[0] == '-' {
			flags = append(flags, a)
			continue
		}
		operands = append(operands, a)
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
	fmt.Fprintln(out, "  praxis store inspect <journal>")
	fmt.Fprintln(out, "  praxis store repair  <journal> [--apply] [--discard-corrupt-batch]")
}

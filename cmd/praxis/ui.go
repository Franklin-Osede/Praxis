package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"praxis/internal/adapters/ui"
	"praxis/internal/challenge"
	"praxis/internal/market"
	"praxis/internal/session"
)

// uiCommand serves one session to one person on this machine.
//
// It prints the address rather than opening a browser. A window nobody asked
// for is a step nobody recorded, and in a study the difference matters.
func uiCommand(args []string, out, errOut *os.File) int {
	fs := flag.NewFlagSet("praxis ui", flag.ContinueOnError)
	fs.SetOutput(errOut)
	journalPath := fs.String("journal", "", "path to the journal to write or resume (required)")
	subject := fs.String("subject", "", "the label the protocol assigns this participant, for a new journal")
	runID := fs.String("run-id", "", "the label the operator gives this execution, for a new journal")
	addr := fs.String("addr", "127.0.0.1:0", "loopback address to listen on")
	startingBalance := fs.Int64("starting-balance", 0, "starting balance in cents, for a new journal")
	commission := fs.Int64("commission", 0, "commission per contract in cents, for a new journal")
	maxDailyLoss := fs.Int64("max-daily-loss", 0, "daily loss limit in cents, for a new journal")
	profitTarget := fs.Int64("profit-target", 0, "profit target in cents, for a new journal")
	maxTotalLoss := fs.Int64("max-total-loss", 0, "static drawdown in cents, for a new journal")
	trailing := fs.Int64("trailing-drawdown", 0, "trailing drawdown in cents, for a new journal")

	flags, operands := splitArgs(fs, args)
	if err := fs.Parse(flags); err != nil || len(operands) != 1 || *journalPath == "" {
		fmt.Fprintln(errOut, "usage: praxis ui <market-file> --journal <journal> --subject <label> --run-id <label> [configuration]")
		fs.PrintDefaults()
		return exitFatal
	}

	// The handover key is printed here and nowhere else: it is what lets the
	// controls be taken from a tab that is gone, so it belongs to whoever sits at
	// this console. Each transfer spends it and the next one is printed.
	// Called from the opening goroutine and then from request handlers, so the
	// count it keeps is held under a lock.
	var (
		printing  sync.Mutex
		handovers int
	)
	server, err := ui.Open(ui.Options{
		Market: operands[0], Journal: *journalPath, Addr: *addr,
		Handover: func(key string) {
			printing.Lock()
			defer printing.Unlock()
			if handovers == 0 {
				fmt.Fprintf(out, "handover: %s  (takes the controls from a tab that is gone; works once)\n", key)
			} else {
				fmt.Fprintf(out, "handover: %s  (the previous key was used to take the controls)\n", key)
			}
			handovers++
		},
		New: session.Config{
			SubjectID: *subject,
			// Supplied, never minted. A value from crypto/rand would end the
			// byte-for-byte identity of two runs that four determinism tests
			// rest on, so the operator names the run and writes the name down
			// beside the anchor — see docs/experiment/pilot-protocol.md.
			RunID: *runID,
			// This interface produces pilot journals and says so in the
			// configuration, which is what puts it inside the digest a
			// pre-registration records.
			Pacing:                   session.PacingPilot,
			StartingBalanceCts:       market.Cents(*startingBalance),
			CommissionPerContractCts: market.Cents(*commission),
			Rules: challenge.Rules{
				StartingBalanceCts:  market.Cents(*startingBalance),
				MaxDailyLossCts:     market.Cents(*maxDailyLoss),
				ProfitTargetCts:     market.Cents(*profitTarget),
				MaxTotalLossCts:     market.Cents(*maxTotalLoss),
				TrailingDrawdownCts: market.Cents(*trailing),
			},
		},
	})
	if errors.Is(err, ui.ErrDamagedTail) {
		fmt.Fprintf(errOut, "praxis: %v\n", err)
		fmt.Fprintf(errOut, "run: praxis store inspect %s\n", *journalPath)
		return exitRepairable
	}
	if code, handled := classify(err, errOut); handled {
		return code
	}
	if err != nil {
		fmt.Fprintf(errOut, "praxis: %v\n", err)
		return exitFatal
	}
	defer server.Close()

	fmt.Fprintf(out, "market:  %s\n", operands[0])
	fmt.Fprintf(out, "journal: %s\n", *journalPath)
	fmt.Fprintf(out, "open:    %s\n", server.Origin())

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	served := make(chan error, 1)
	go func() { served <- server.Serve() }()

	select {
	case <-stop:
		return exitClean
	case err := <-served:
		if err != nil {
			fmt.Fprintf(errOut, "praxis: %v\n", err)
			return exitFatal
		}
		return exitClean
	}
}

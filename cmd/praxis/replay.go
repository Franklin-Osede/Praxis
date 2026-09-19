package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"praxis/internal/adapters/marketdata"
	"praxis/internal/adapters/persistence"
	"praxis/internal/challenge"
	"praxis/internal/market"
	"praxis/internal/session"
)

// errHumanPaced reports a journal produced by a person through the interface.
// It is this command's refusal and not the kernel's: the session is willing to
// take an observation, and what must not happen is this feed supplying one.
var errHumanPaced = errors.New("praxis: this journal was traded by somebody")

// replayCommand runs or resumes a simulation over a market file.
//
// "replay" here means replaying the market inside a session, which is not what
// reading an event journal means. `praxis replay` runs or resumes a
// simulation; `praxis store inspect` examines the history one left behind.
func replayCommand(args []string, out, errOut *os.File) int {
	fs := flag.NewFlagSet("praxis replay", flag.ContinueOnError)
	fs.SetOutput(errOut)
	journalPath := fs.String("journal", "", "path to the journal to write or resume (required)")
	startingBalance := fs.Int64("starting-balance", 0, "starting balance in cents, for a new journal")
	commission := fs.Int64("commission", 0, "commission per contract in cents, for a new journal")
	maxDailyLoss := fs.Int64("max-daily-loss", 0, "daily loss limit in cents, for a new journal")
	profitTarget := fs.Int64("profit-target", 0, "profit target in cents, for a new journal")
	maxTotalLoss := fs.Int64("max-total-loss", 0, "static drawdown in cents, for a new journal")
	trailing := fs.Int64("trailing-drawdown", 0, "trailing drawdown in cents, for a new journal")

	flags, operands := splitArgs(fs, args)
	if err := fs.Parse(flags); err != nil || len(operands) != 1 || *journalPath == "" {
		fmt.Fprintln(errOut, "usage: praxis replay <market-file> --journal <journal> [configuration]")
		fs.PrintDefaults()
		return exitFatal
	}

	feed, err := marketdata.ReadFile(operands[0])
	if err != nil {
		if code, handled := classify(err, errOut); handled {
			return code
		}
	}

	// One lock, taken here and held through recovery and every commit. Reading
	// the journal and then opening a writer would leave a window in which
	// another process could take it.
	writer, err := persistence.OpenWriter(*journalPath, persistence.DurableEveryBatch)
	if errors.Is(err, persistence.ErrUnconfirmedTail) {
		fmt.Fprintf(errOut, "praxis: %v\n", err)
		fmt.Fprintf(errOut, "run: praxis store inspect %s\n", *journalPath)
		return exitRepairable
	}
	if code, handled := classify(err, errOut); handled {
		return code
	}
	defer writer.Close()

	recovered := writer.Recovered()
	resumed := len(recovered.Batches) > 0

	var (
		s        *session.Session
		consumed int
	)
	if resumed {
		s, consumed, err = resume(recovered, feed, writer)
	} else {
		s, err = start(feed, writer, session.Config{
			Instrument:               feed.Instrument,
			StartingBalanceCts:       market.Cents(*startingBalance),
			CommissionPerContractCts: market.Cents(*commission),
			Rules: challenge.Rules{
				StartingBalanceCts:  market.Cents(*startingBalance),
				MaxDailyLossCts:     market.Cents(*maxDailyLoss),
				ProfitTargetCts:     market.Cents(*profitTarget),
				MaxTotalLossCts:     market.Cents(*maxTotalLoss),
				TrailingDrawdownCts: market.Cents(*trailing),
			},
		})
	}
	if err != nil {
		fmt.Fprintf(errOut, "praxis: %v\n", err)
		return exitFatal
	}

	batchesBefore := writer.NextBatchNumber()
	if err := marketdata.Drive(s, feed, consumed); err != nil {
		if errors.Is(err, session.ErrSessionNeedsRecovery) {
			fmt.Fprintf(errOut, "praxis: %v\n", err)
			fmt.Fprintf(errOut, "the outcome of the last command is unknown; it was not retried.\n")
			fmt.Fprintf(errOut, "run: praxis store inspect %s\n", *journalPath)
			return exitRepairable
		}
		fmt.Fprintf(errOut, "praxis: %v\n", err)
		return exitFatal
	}

	// What the journal actually holds, not what the file offers: a run that
	// stopped because the evaluation ended consumed less than the whole file,
	// and saying otherwise would report rows nobody took.
	held, err := marketdata.Consumed(s.Events(), feed)
	if err != nil {
		fmt.Fprintf(errOut, "praxis: %v\n", err)
		return exitFatal
	}

	fmt.Fprintf(out, "market:    %s\n", operands[0])
	fmt.Fprintf(out, "journal:   %s\n", *journalPath)
	fmt.Fprintf(out, "verified:  %d rows already in the journal\n", consumed)
	fmt.Fprintf(out, "consumed:  %d new rows\n", held-consumed)
	fmt.Fprintf(out, "batches:   %d confirmed this run\n", writer.NextBatchNumber()-batchesBefore)
	fmt.Fprintf(out, "sequence:  last event %d\n", writer.NextSequence()-1)
	fmt.Fprintf(out, "challenge: %v\n", s.Challenge().State())
	if reason := s.Challenge().FailureReason(); reason != challenge.FailureNone {
		fmt.Fprintf(out, "reason:    %v\n", reason)
	}
	// The end of a file is not a session boundary, so the last trading session
	// is left open rather than closed on its behalf.
	// An evaluation that ends is a result, not a failure, so the run is clean
	// and the outcome is a line rather than an exit code. That means "the file
	// was consumed whole" is no longer deducible from the status, which is why
	// it is stated: anything automating that question reads this line.
	if remaining := len(feed.Observations) - held; remaining > 0 {
		fmt.Fprintf(out, "remaining: %d market rows not consumed\n", remaining)
	}
	if s.TradingSessionOpen() {
		fmt.Fprintf(out, "open:      trading session %s is still open\n", s.OpenSessionID())
	}
	return exitClean
}

// start builds a session for a journal that has nothing in it yet.
//
// It takes the committer the session writes through rather than the writer
// itself. The command always passes its writer; a test passes one that fails a
// chosen commit, so that recovering from it is tested along this path and not a
// copy of it.
func start(feed *marketdata.Feed, committer session.BatchCommitter, cfg session.Config) (*session.Session, error) {
	if cfg.StartingBalanceCts <= 0 {
		return nil, errors.New("a new journal needs --starting-balance and the rules it will be evaluated against")
	}
	at := feed.Observations[0].Quote.Time
	return session.New(cfg, at, committer)
}

// resume continues a journal, refusing one that does not describe this file.
func resume(recovered *persistence.Journal, feed *marketdata.Feed, committer session.BatchCommitter) (*session.Session, int, error) {
	events := recovered.Events()
	if err := session.Verify(events); err != nil {
		return nil, 0, err
	}
	state, err := session.Replay(events)
	if err != nil {
		return nil, 0, err
	}
	// A journal somebody traded is not this command's to continue. The feed
	// here is scripted: it advances the market with nobody watching, so every
	// row it added would be one no participant was shown, and the
	// presentations the journal holds would stop matching its observations —
	// a hole in the one quantity the pilots record, in a journal that still
	// verifies clean afterwards. Recovery for those runs is praxis ui, which
	// resumes the same journal and waits for a person.
	if state.Config.Pacing != session.PacingScripted {
		return nil, 0, fmt.Errorf("%w: it was traded at %v pacing; resume it with praxis ui",
			errHumanPaced, state.Config.Pacing)
	}
	// Configuration comes from the journal, never from the flags. A resumed
	// run cannot be reconfigured: the account and the evaluation already have
	// a history under the rules recorded when they started.
	if state.Config.Instrument != feed.Instrument {
		return nil, 0, fmt.Errorf("this journal is for %s at %d cents per tick; the file is %s at %d",
			state.Config.Instrument.Symbol, state.Config.Instrument.CentsPerTick,
			feed.Instrument.Symbol, feed.Instrument.CentsPerTick)
	}
	consumed, err := marketdata.Consumed(events, feed)
	if err != nil {
		return nil, 0, err
	}
	s, err := session.Resume(state, committer)
	return s, consumed, err
}

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"praxis/internal/adapters/marketdata"
	"praxis/internal/adapters/persistence"
	"praxis/internal/challenge"
	"praxis/internal/session"
)

// A commit that fails leaves the session needing recovery, and by ADR-012 that
// never heals: the session that failed is never asked to go on. What goes on is
// a new one, recovered from the confirmed batches, continuing from the row the
// journal says it reached. So re-entering a step is not calling Step again on a
// poisoned session — it is this path, and it is tested here as the replay
// command walks it, through the same start and resume functions.
//
// A failed commit's outcome is unknown until the journal is read: the batch may
// have reached the disk before the error was reported, or not. Both are tried,
// at every commit of a run that crosses boundaries, and every one must recover
// to the journal an uninterrupted run writes, byte for byte. A duplicated
// SessionEnded or SessionOpened anywhere would change the bytes, so the property
// finds it without the case having to be named.

var errCommitLost = errors.New("injected: the commit's outcome was not reported")

// failingAt passes every commit to a real writer except the one numbered at. For
// that one it either writes the batch and then reports failure — it landed — or
// reports failure without writing it.
type failingAt struct {
	writer *persistence.Writer
	at     int
	landed bool

	seen  int
	fired bool
}

func (c *failingAt) Commit(events []session.Event) error {
	c.seen++
	if c.seen != c.at {
		return c.writer.Commit(events)
	}
	c.fired = true
	if c.landed {
		if err := c.writer.Commit(events); err != nil {
			return err
		}
	}
	return errCommitLost
}

// counting records, for each commit of a run, the kind of the first event in it,
// so the property can say which boundary commands it actually covered.
type counting struct {
	writer *persistence.Writer
	kinds  []session.Kind
}

func (c *counting) Commit(events []session.Event) error {
	c.kinds = append(c.kinds, events[0].Header().Kind)
	return c.writer.Commit(events)
}

// threeSessions crosses two boundaries, so every boundary command appears more
// than once among the commits.
const threeSessions = marketFile + "7000,1,d3,19980,19981,10,10\n"

func recoveryConfig(feed *marketdata.Feed) session.Config {
	return session.Config{
		Instrument:               feed.Instrument,
		StartingBalanceCts:       5_000_000,
		CommissionPerContractCts: 50,
		Rules: challenge.Rules{
			StartingBalanceCts: 5_000_000,
			MaxDailyLossCts:    100_000,
			ProfitTargetCts:    300_000,
		},
	}
}

// replayOver is the replay command's own sequence, over any committer: open the
// journal, start or resume it, and drive from the row it reached.
func replayOver(t *testing.T, path string, feed *marketdata.Feed, wrap func(*persistence.Writer) session.BatchCommitter) error {
	t.Helper()
	writer, err := persistence.OpenWriter(path, persistence.DurableEveryBatch)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	defer writer.Close()

	var committer session.BatchCommitter = writer
	if wrap != nil {
		committer = wrap(writer)
	}
	recovered := writer.Recovered()

	var (
		s        *session.Session
		consumed int
	)
	if len(recovered.Batches) > 0 {
		s, consumed, err = resume(recovered, feed, committer)
	} else {
		s, err = start(feed, committer, recoveryConfig(feed))
	}
	if err != nil {
		return err
	}
	return marketdata.Drive(s, feed, consumed)
}

// Scenario: a commit fails at every point of a run, landed or not, and recovery
// writes the journal an uninterrupted run writes
func TestARunRecoversFromAFailedCommitAnywhereToTheSameJournal(t *testing.T) {
	dir := t.TempDir()
	marketPath := writeMarket(t, dir, "market.csv", threeSessions)
	feed, err := marketdata.ReadFile(marketPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	whole := filepath.Join(dir, "whole.praxis")
	count := &counting{}
	if err := replayOver(t, whole, feed, func(w *persistence.Writer) session.BatchCommitter {
		count.writer = w
		return count
	}); err != nil {
		t.Fatalf("the uninterrupted run failed: %v", err)
	}
	want, err := os.ReadFile(whole)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// The property is only as strong as the commits it reaches, so the ones it
	// reaches are checked rather than assumed: every boundary command, and an
	// observation, must be among them.
	covered := map[session.Kind]bool{}
	for _, k := range count.kinds {
		covered[k] = true
	}
	for _, kind := range []session.Kind{
		session.KindSessionStarted, session.KindSessionOpened,
		session.KindSessionEnded, session.KindMarketObserved,
	} {
		if !covered[kind] {
			t.Fatalf("no commit of the run begins with %v, so a failure there is never tried", kind)
		}
	}

	for at := 1; at <= len(count.kinds); at++ {
		for _, landed := range []bool{false, true} {
			name := count.kinds[at-1].String() + "/lost"
			if landed {
				name = count.kinds[at-1].String() + "/landed"
			}
			t.Run(strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "journal.praxis")

				failing := &failingAt{at: at, landed: landed}
				err := replayOver(t, path, feed, func(w *persistence.Writer) session.BatchCommitter {
					failing.writer = w
					return failing
				})
				if !failing.fired {
					t.Fatalf("commit %d was never reached", at)
				}
				if err == nil {
					t.Fatalf("commit %d failed and the run reported success", at)
				}

				if err := replayOver(t, path, feed, nil); err != nil {
					t.Fatalf("recovering after commit %d: %v", at, err)
				}
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("ReadFile: %v", err)
				}
				if string(got) != string(want) {
					t.Fatalf("recovering after commit %d (%s) wrote a different journal", at, name)
				}
			})
		}
	}
}

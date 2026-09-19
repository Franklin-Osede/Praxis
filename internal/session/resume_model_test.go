package session_test

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"testing"

	"praxis/internal/challenge"
	"praxis/internal/market"
	"praxis/internal/session"
)

var atkInstr = market.Instrument{Symbol: "MNQ", CentsPerTick: 50}

func atkConfig() session.Config {
	return session.Config{
		Instrument: atkInstr, SubjectID: "t-01", RunID: "r-01", Pacing: session.PacingPilot,
		StartingBalanceCts: 5_000_000, CommissionPerContractCts: 50,
		Rules: challenge.Rules{
			StartingBalanceCts: 5_000_000, MaxDailyLossCts: 3_000, ProfitTargetCts: 4_000,
			MaxTotalLossCts: 6_000, TrailingDrawdownCts: 5_000,
		},
	}
}

// driver interprets a byte program as commands. Its state is only the program
// position and counters derived from it, so the same driver state reproduces the
// same next command against any session in the same state.
type driver struct {
	prog    []byte
	pc      int
	time    market.LogicalTime
	mid     market.Ticks
	days    int
	orders  int
	elapsed session.ElapsedNanos
	gesture int
	srcSeq  uint64
}

func (d *driver) b() int {
	if d.pc >= len(d.prog) {
		return 0
	}
	v := int(d.prog[d.pc])
	d.pc++
	return v
}

func (d *driver) done() bool { return d.pc >= len(d.prog) }

func (d *driver) decision() session.Decision {
	d.gesture++
	d.elapsed += 1_000
	return session.Decision{GestureID: "g-" + strconv.Itoa(d.gesture), AtUTCNanos: 1_764_000_000_000_000_000, Segment: 1, ElapsedNanos: d.elapsed}
}

func side(v int) market.Side {
	if v%2 == 0 {
		return market.SideBuy
	}
	return market.SideSell
}

// step runs one command and names it.
func (d *driver) step(s *session.Session) (string, error) {
	op := d.b() % 11
	switch op {
	case 0, 1, 2:
		d.time += market.LogicalTime(d.b() % 3)
		d.mid += market.Ticks(d.b()%9 - 4)
		spread := market.Ticks(d.b()%2 + 1)
		q := market.Quote{Instrument: atkInstr, Time: d.time, Bid: d.mid, Ask: d.mid + spread,
			BidSize: market.Qty(d.b() % 4), AskSize: market.Qty(d.b() % 4)}
		d.srcSeq++
		return fmt.Sprintf("observe %+v", q), s.Observe(q, d.srcSeq)
	case 3:
		d.time += market.LogicalTime(d.b() % 2)
		d.days++
		return "open", s.OpenTradingSession(d.time, challenge.SessionID("d"+strconv.Itoa(d.days)))
	case 4:
		d.time += market.LogicalTime(d.b() % 2)
		return "end", s.EndTradingSession(d.time)
	case 5:
		id, waiting := s.Pending(1)
		d.elapsed += 1_000
		if !waiting {
			return "present(nothing)", nil
		}
		return "present", s.AcknowledgePresentation(id, session.Instant{AtUTCNanos: 1, Segment: 1, ElapsedNanos: d.elapsed})
	case 6, 7:
		d.orders++
		id := "o-" + strconv.Itoa(d.orders)
		sd, qty := side(d.b()), market.Qty(d.b()%3+1)
		off := market.Ticks(d.b()%7 - 3)
		var o market.Order
		var err error
		switch d.b() % 3 {
		case 0:
			o, err = market.NewMarketOrder(id, atkInstr, sd, qty)
		case 1:
			o, err = market.NewLimitOrder(id, atkInstr, sd, qty, d.mid+off)
		default:
			o, err = market.NewStopOrder(id, atkInstr, sd, qty, d.mid+off)
		}
		if err != nil {
			return "bad order", nil
		}
		if op == 7 {
			stopOff, tgtOff := market.Ticks(d.b()%5+1), market.Ticks(d.b()%5+1)
			stop, target := d.mid-stopOff, d.mid+tgtOff
			if sd == market.SideSell {
				stop, target = d.mid+stopOff, d.mid-tgtOff
			}
			if d.b()%3 == 0 {
				target = 0
			}
			return fmt.Sprintf("submit+protect %+v %d %d", o, stop, target), s.SubmitOrderWithProtection(o, stop, target, d.decision())
		}
		return fmt.Sprintf("submit %+v", o), s.SubmitOrder(o, d.decision())
	case 8:
		w := s.WorkingOrders()
		pick := d.b()
		if len(w) == 0 {
			return "cancel(nothing)", nil
		}
		return "cancel " + w[pick%len(w)].ID, s.CancelOrder(w[pick%len(w)].ID, d.decision())
	case 9, 10:
		var refs []session.ProtectionRef
		for _, p := range s.PlannedProtections() {
			refs = append(refs, session.ProtectionRef{Kind: session.ProtectionRefEntry, OrderID: p.EntryOrderID})
		}
		for _, a := range s.ActiveProtections() {
			refs = append(refs, session.ProtectionRef{Kind: session.ProtectionRefEpisode, EpisodeID: a.EpisodeID})
		}
		pick, so, to := d.b(), market.Ticks(d.b()%7+1), market.Ticks(d.b()%7+1)
		if len(refs) == 0 {
			return "protect(nothing)", nil
		}
		ref := refs[pick%len(refs)]
		if op == 10 {
			return fmt.Sprintf("withdraw %+v", ref), s.CancelProtection(ref, d.decision())
		}
		long := true
		if p, ok := s.Account().Position(atkInstr); ok && p.NetQty < 0 {
			long = false
		}
		for _, pl := range s.WorkingOrders() {
			if ref.Kind == session.ProtectionRefEntry && pl.ID == ref.OrderID {
				long = pl.Side == market.SideBuy
			}
		}
		stop, target := d.mid-so, d.mid+to
		if !long {
			stop, target = d.mid+so, d.mid-to
		}
		return fmt.Sprintf("replace %+v %d %d", ref, stop, target), s.ReplaceProtection(ref, stop, target, d.decision())
	}
	return "noop", nil
}

type outcome struct {
	names  []string
	errs   []string
	events []session.Event
	cuts   []int         // journal length after each successful command that produced events
	states []driver      // driver state at each cut
}

func errClass(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func runProgram(t *testing.T, d driver, s *session.Session, limit int) outcome {
	var out outcome
	for n := 0; n < limit && !d.done(); n++ {
		before := s.JournalLen()
		name, err := d.step(s)
		out.names = append(out.names, name)
		out.errs = append(out.errs, errClass(err))
		if errors.Is(err, session.ErrSessionNeedsRecovery) {
			break
		}
		if err == nil && s.JournalLen() > before {
			out.cuts = append(out.cuts, s.JournalLen())
			out.states = append(out.states, d)
		}
	}
	out.events = s.Events()
	return out
}

func checkProgram(t *testing.T, prog []byte) {
	s, err := session.New(atkConfig(), 1_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := driver{prog: prog, time: 1_000, mid: 20_000}
	full := runProgram(t, start, s, 1<<30)
	if n := len(full.errs); n > 0 && errors.Is(s.NeedsRecovery(), s.NeedsRecovery()) && s.NeedsRecovery() != nil {
		t.Fatalf("a command left the in-memory session needing recovery: %s -> %v\nprogram %v", full.names[n-1], s.NeedsRecovery(), prog)
	}
	if err := session.Verify(full.events); err != nil {
		t.Fatalf("Verify refused a journal the live session wrote: %v\ncommands %q\nerrs %q", err, full.names, full.errs)
	}
	if _, err := session.Replay(full.events); err != nil {
		t.Fatalf("Replay refused a journal the live session wrote: %v\ncommands %q\nerrs %q", err, full.names, full.errs)
	}
	for k, cut := range full.cuts {
		state, err := session.Replay(full.events[:cut])
		if err != nil {
			t.Fatalf("Replay of prefix %d: %v", cut, err)
		}
		resumed, err := session.Resume(state, nil)
		if err != nil {
			t.Fatalf("Resume at %d: %v", cut, err)
		}
		rest := runProgram(t, full.states[k], resumed, 1<<30)
		if !reflect.DeepEqual(rest.events, full.events) {
			n := 0
			for n < len(rest.events) && n < len(full.events) && reflect.DeepEqual(rest.events[n], full.events[n]) {
				n++
			}
			var got, want session.Event
			if n < len(rest.events) {
				got = rest.events[n]
			}
			if n < len(full.events) {
				want = full.events[n]
			}
			t.Fatalf("resumed after event %d diverges at event %d:\n got %#v\nwant %#v\nuninterrupted %q\n %q\nresumed %q\n %q",
				cut, n, got, want, full.names, full.errs, rest.names, rest.errs)
		}
	}
}

// FuzzResumeAtEveryBatch drives a session through arbitrary programs of
// observations, boundaries, presentations, orders, protection, cancellations,
// replacements and withdrawals, then replays and resumes at every batch and
// demands the same stream byte for byte. Cutting at every batch is what covers
// a failed commit landing or not landing at any position.
//
// It arrived with the replacement defect it found: a stop moved to where the
// market already was left a journal the live session accepted and Replay
// refused. It is kept because that class of defect — the writer and the reader
// disagreeing about what the market had already done — is invisible to a test
// that checks either side alone.
func FuzzResumeAtEveryBatch(f *testing.F) {
	f.Add([]byte{3, 0, 0, 0, 0, 2, 2, 5, 6, 0, 1, 0, 2, 0, 3, 3, 1, 1, 5, 7, 1, 2, 3, 1, 2, 2, 0, 2, 1, 2, 3, 3, 5, 8, 0, 4, 1})
	f.Add([]byte{3, 0, 0, 0, 3, 1, 3, 3, 5, 7, 0, 1, 3, 2, 3, 3, 0, 2, 0, 2, 1, 3, 3, 5, 9, 0, 3, 3, 0, 1, 0, 1, 2, 2, 4, 0, 3, 0})
	f.Fuzz(func(t *testing.T, prog []byte) {
		if len(prog) > 300 {
			return
		}
		checkProgram(t, prog)
	})
}

func newAtk(t *testing.T) *session.Session {
	s, err := session.New(atkConfig(), 1_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}


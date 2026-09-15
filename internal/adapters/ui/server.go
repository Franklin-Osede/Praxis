package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"

	"praxis/internal/adapters/marketdata"
	"praxis/internal/adapters/persistence"
	"praxis/internal/market"
	"praxis/internal/session"
)

// Errors reported when a journal cannot be driven by a person.
var (
	// ErrNotTraded reports a journal produced by a script. The interface must
	// not quietly turn it into somebody's: the pacing and the subject are one
	// claim, and adding human decisions to a run nobody made would be
	// fabricating the sample.
	ErrNotTraded = errors.New("ui: this journal was not traded by anyone, and this interface cannot make it so")

	// ErrConfirmatoryNotBuilt reports a journal that says it was produced under
	// a fixed automatic cadence, which does not exist yet. Serving it from the
	// pilot interface would mean recording it as confirmatory while a person
	// advanced it by hand.
	ErrConfirmatoryNotBuilt = errors.New("ui: confirmatory pacing is not implemented, and this journal claims it")

	// ErrUnwritableVersion reports a journal in a payload version with no
	// place to record when a person decided anything. It stays readable; it
	// cannot be continued from here.
	ErrUnwritableVersion = errors.New("ui: this journal's payload version cannot record a human decision")

	// ErrDamagedTail reports a journal whose last batch is not confirmed.
	// Repair is never automatic: it is a decision about which bytes to
	// discard, and it belongs to a person with a command line.
	ErrDamagedTail = errors.New("ui: this journal has an unconfirmed tail; run praxis store inspect")
)

// Options are what a server is opened over.
type Options struct {
	// Journal is written and resumed under one lock, held for the life of the
	// server.
	Journal string

	// Market is the canonical file the observations come from. A journal that
	// does not describe this file is refused rather than continued.
	Market string

	// Addr is where to listen. It is forced to loopback: a local server can
	// still be reached by a page open in the browser, so binding every
	// interface would put an unauthenticated kernel on the network.
	Addr string

	// Now is the clock the server stamps with. Nil means SystemClock.
	//
	// It is injected rather than called, and that is what makes a run
	// reproducible: the same commands with the same clock produce the same
	// journal, which is how a journal written through HTTP can be compared
	// byte for byte with one written directly. Rule 2 forbids a clock in the
	// domain and allows one here; injecting it is the difference between an
	// adapter that can be tested and one that cannot.
	Now Clock

	// Handover is how the operator is shown the key that lets the controls be
	// taken from whoever holds them. It is called once when the server opens and
	// again after every transfer, with the key that replaces the one just spent.
	//
	// Nil leaves the key unshown, which fails closed: the controls can still be
	// taken when nobody holds them, and never from someone who does.
	Handover func(key string)

	// New is the configuration for a journal that does not exist yet. It is
	// ignored for one that does — a resumed run cannot be reconfigured,
	// because its account and evaluation already have a history.
	New session.Config
}

// Server owns one session and gives exactly one client the controls.
type Server struct {
	writer  *persistence.Writer
	session *session.Session
	feed    *marketdata.Feed
	cursor  int
	cfg     session.Config

	// lastStep is the last step this server applied, and it is touched only on
	// the loop. See stepRecord.
	lastStep stepRecord

	lease *lease

	// commands is the only way to the kernel. Handlers post to it and wait;
	// the loop is the single goroutine that touches the session, so the
	// kernel's inputs arrive in one order however many sockets are open.
	commands  chan func()
	now       Clock
	done      chan struct{}
	closeOnce sync.Once

	listener net.Listener
	http     *http.Server
	origin   string

	handover func(key string)
}

// Open recovers a journal, refuses one this interface must not drive, and takes
// the controls with nobody holding them.
//
// One lock, taken here and held for the life of the server. Reading the journal
// and then opening a writer would leave a window in which another process could
// take it.
func Open(opts Options) (*Server, error) {
	feed, err := marketdata.ReadFile(opts.Market)
	if err != nil {
		return nil, err
	}

	writer, err := persistence.OpenWriter(opts.Journal, persistence.DurableEveryBatch)
	if errors.Is(err, persistence.ErrUnconfirmedTail) {
		return nil, fmt.Errorf("%w: %v", ErrDamagedTail, err)
	}
	if err != nil {
		return nil, err
	}

	now := opts.Now
	if now == nil {
		now = SystemClock()
	}
	s := &Server{writer: writer, feed: feed, now: now,
		commands: make(chan func()), done: make(chan struct{})}
	if err := s.start(opts, feed); err != nil {
		writer.Close()
		return nil, err
	}

	addr := opts.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	if err := s.listen(addr); err != nil {
		writer.Close()
		return nil, err
	}
	key, err := s.lease.mintHandover()
	if err != nil {
		writer.Close()
		return nil, err
	}
	s.handover = opts.Handover
	if s.handover != nil {
		s.handover(key)
	}
	go s.loop()
	return s, nil
}

// start builds or resumes the session, and refuses a journal the interface must
// not continue.
func (s *Server) start(opts Options, feed *marketdata.Feed) error {
	recovered := writerJournal(s.writer)
	if len(recovered.Batches) == 0 {
		return s.begin(opts, feed)
	}
	return s.resume(recovered, feed)
}

func writerJournal(w *persistence.Writer) *persistence.Journal { return w.Recovered() }

func (s *Server) begin(opts Options, feed *marketdata.Feed) error {
	cfg := opts.New
	cfg.Instrument = feed.Instrument
	if err := usableHere(cfg, persistence.EventVersion); err != nil {
		return err
	}
	built, err := session.New(cfg, feed.Observations[0].Quote.Time, s.writer)
	if err != nil {
		return err
	}
	s.session, s.cfg, s.cursor = built, cfg, 0
	s.lease = newLease(0, s.now)
	return nil
}

func (s *Server) resume(recovered *persistence.Journal, feed *marketdata.Feed) error {
	events := recovered.Events()
	if err := session.Verify(events); err != nil {
		return err
	}
	state, err := session.Replay(events)
	if err != nil {
		return err
	}
	if err := usableHere(state.Config, recovered.PayloadVersion); err != nil {
		return err
	}
	if state.Config.Instrument != feed.Instrument {
		return fmt.Errorf("%w: the journal trades %s, the file carries %s",
			marketdata.ErrFeedMismatch, state.Config.Instrument.Symbol, feed.Instrument.Symbol)
	}
	// Every observation the journal holds is compared with the row in the same
	// position, field by field. Skipping a count of rows would replay a file
	// that changed while keeping its length as though it were the one that
	// produced the journal.
	consumed, err := marketdata.Consumed(events, feed)
	if err != nil {
		return err
	}
	resumed, err := session.Resume(state, s.writer)
	if err != nil {
		return err
	}
	s.session, s.cfg, s.cursor = resumed, state.Config, consumed

	// A restart continues above every segment the journal already holds, so
	// that a lease never reuses a number anything in it is stamped with. The
	// chronology is asked rather than the commands counted: a run in which the
	// person only advanced and confirmed stamps presentations and no command,
	// and counting commands granted that run's number a second time.
	s.lease = newLease(state.InteractionSegment(), s.now)
	return nil
}

// usableHere refuses a journal this interface must not add human decisions to.
func usableHere(cfg session.Config, version string) error {
	switch cfg.Pacing {
	case session.PacingScripted:
		return fmt.Errorf("%w: %s", ErrNotTraded, describeSubject(cfg))
	case session.PacingConfirmatory:
		return ErrConfirmatoryNotBuilt
	}
	// Unreachable today, and kept for when it is not. Only the current payload
	// version can express a pacing mode at all, so every journal in an older
	// one is scripted and refused above. This becomes the reachable check the
	// first time two writable versions exist at once.
	if version != persistence.EventVersion {
		return fmt.Errorf("%w: %s", ErrUnwritableVersion, version)
	}
	return nil
}

func describeSubject(cfg session.Config) string {
	if cfg.SubjectID == "" {
		return "nobody is recorded as having traded it"
	}
	return cfg.SubjectID
}

func (s *Server) listen(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("ui: %q is not host:port: %w", addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("ui: %q is not a loopback address; this server is never exposed", host)
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.listener = l
	s.origin = "http://" + l.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/control", s.handleControl)
	mux.HandleFunc("/api/acknowledge", s.handleAcknowledge)
	mux.HandleFunc("/api/step", s.handleStep)
	mux.HandleFunc("/api/command", s.handleCommand)
	s.http = &http.Server{Handler: s.guard(mux)}
	return nil
}

// Addr is where the server is listening, printed rather than opened: a browser
// launched automatically is a second window nobody asked for and, in a study,
// a step nobody recorded.
func (s *Server) Addr() string { return s.listener.Addr().String() }

// Origin is what this server calls itself, and the only one it answers to.
func (s *Server) Origin() string { return s.origin }

// Serve runs until Close.
func (s *Server) Serve() error {
	err := s.http.Serve(s.listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close stops serving, stops the loop and releases the journal's lock. It is
// safe to call more than once, because it is called more than once: from a
// defer, from a signal handler, and from a test's cleanup, any two of which
// can run. A close of a closed channel is a
// panic that takes the process down while it is shutting down cleanly — which
// is the one moment a journal is most likely to be mid-commit.
func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		if s.http != nil {
			s.http.Close()
		}
		close(s.done)
		err = s.writer.Close()
	})
	return err
}

// loop is the only goroutine that touches the session.
func (s *Server) loop() {
	for {
		select {
		case run := <-s.commands:
			run()
		case <-s.done:
			return
		}
	}
}

// ask runs one function on the loop and waits for it. Handlers never reach the
// session directly, so two requests arriving at once are serialised by the
// kernel's single owner rather than by hope.
func (s *Server) ask(run func()) error {
	finished := make(chan struct{})
	select {
	case s.commands <- func() { run(); close(finished) }:
		<-finished
		return nil
	case <-s.done:
		return errors.New("ui: the server is closed")
	}
}

// guard refuses what a local server must refuse.
//
// "Only local" is not a boundary. A malicious page open in the same browser can
// reach 127.0.0.1, so an Origin that is not this server's is refused outright,
// and commands are POST only — a form or an image tag can issue a cross-site
// GET without any script at all.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && origin != s.origin {
			http.Error(w, "ui: this server answers only its own origin", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/state" && r.Method != http.MethodPost {
			http.Error(w, "ui: commands are POST", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	var state State
	if err := s.ask(func() { state = s.state() }); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// control is what a client is given when it takes the controls.
type control struct {
	Lease   string `json:"lease"`
	Segment string `json:"segment"`
	State   State  `json:"state"`
}

func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	// The loop answers first, and the lease is taken only once it has. A
	// segment is spent to say "a new run of uninterrupted interaction begins
	// here", and segments only go up: minting one for a request that is then
	// refused records a run that never happened and leaves the controls held
	// by a client that was told it failed.
	var state State
	if err := s.ask(func() { state = s.state() }); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	var (
		token   string
		segment uint64
		err     error
	)
	if r.URL.Query().Get("transfer") == "yes" {
		var body struct {
			Handover string `json:"handover"`
		}
		// An absent or unreadable body presents no key, and is refused as one.
		_ = json.NewDecoder(r.Body).Decode(&body)
		var next string
		token, segment, next, err = s.lease.transfer(body.Handover)
		if errors.Is(err, ErrHandoverRefused) {
			s.refuse(w, err)
			return
		}
		if err == nil && s.handover != nil {
			s.handover(next)
		}
	} else {
		token, segment, err = s.lease.acquire()
	}
	if errors.Is(err, ErrControllerActive) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, control{
		Lease: token, Segment: decimal(int64(segment)), State: state,
	})
}

// state is read on the loop, so it is never a torn view of a session another
// goroutine is changing.
func (s *Server) state() State {
	// The session values itself. A valuation that fails is terminal on the
	// screen and never a figure: showing balance in equity's place would be a
	// number the participant acts on and nothing downstream could tell.
	valuation, err := s.session.Valuation()
	return project(s.session, s.cursor, len(s.feed.Observations), s.cfg, valuation, err).
		withBook(s.lastQuote())
}

// lastQuote is the session's book, not the file's row. The two carry the same
// prices and different sizes: the session consumes the displayed size with
// every fill, so the file's row is what was offered and this is what is left.
//
// s.cursor is no longer the source of the book. It is how far through the file
// this run has read, and it serves Cursor and Observations — the progress the
// participant is shown — and nothing else. Wiring the book back to it would
// undo this without anything looking wrong.
func (s *Server) lastQuote() (market.Quote, bool) { return s.session.LastQuote() }

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

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

	// ErrDeclaredMismatch reports labels supplied for a journal that already
	// names others. See Options.DeclaredSubject.
	ErrDeclaredMismatch = errors.New("ui: this journal names a different run")
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

	// DeclaredSubject and DeclaredRunID are the labels the operator actually
	// typed, empty when they typed none. A resumed journal keeps its own, so
	// these are never read from: they are checked against it, and a
	// disagreement is refused rather than ignored.
	//
	// Silence is not a claim. An operator who names nothing is continuing the
	// run that is there; one who names another run is saying something untrue
	// about it, and the next participant's decisions would be recorded under
	// the last one's label.
	DeclaredSubject string
	DeclaredRunID   string
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

	// stopped is closed by the loop when it returns, and created only by the
	// code that starts it. A server whose loop never started has none, so Close
	// has nothing to wait for rather than waiting forever for a loop that does
	// not exist.
	stopped chan struct{}

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
	s.stopped = make(chan struct{})
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
	return s.resume(opts, recovered, feed)
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

func (s *Server) resume(opts Options, recovered *persistence.Journal, feed *marketdata.Feed) error {
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
	if err := declaresTheSameRun(opts, state.Config); err != nil {
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

// declaresTheSameRun refuses labels that disagree with the journal's own, and
// says both. It is asked before the session opens, so a run that would have
// been recorded under the wrong label never starts.
func declaresTheSameRun(opts Options, cfg session.Config) error {
	for _, claim := range []struct {
		what      string
		declared  string
		inJournal string
	}{
		{"subject", opts.DeclaredSubject, cfg.SubjectID},
		{"run", opts.DeclaredRunID, cfg.RunID},
	} {
		if claim.declared == "" || claim.declared == claim.inJournal {
			continue
		}
		return fmt.Errorf("%w: its %s is %q and %q was supplied; a journal's configuration comes from its own history",
			ErrDeclaredMismatch, claim.what, claim.inJournal, claim.declared)
	}
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
	mux.Handle("/", s.screen())
	s.http = &http.Server{
		Handler: s.guard(mux),
		// A client that opens a connection and sends nothing holds a goroutine
		// and a descriptor until something lets it go. These do.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       60 * time.Second,
		// WriteTimeout is deliberately unset. A response is written after the
		// single loop has run the command, so a deadline on it cuts the answer
		// to a command that was applied: the participant sees a lost response
		// and retries. The retry is safe — that is what a gesture identifier
		// and a step's from-observation are for — but it costs a pilot's
		// attention and buys nothing, because a slow reader blocks its own
		// handler's goroutine and never the loop.
	}
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
//
// It waits for the loop to stop before closing the journal. The loop finishes
// whatever command it already accepted — a command half run is a commit half
// made — and a request that did not get in is refused on the done branch of ask.
// Closing the writer first put a signal arriving mid-commit under the command,
// and its append failed against a closed file.
//
// So Close must never be called from a function the loop is running: it would
// wait for itself. Nothing does, and it cannot be enforced from here — which
// goroutine is calling is not something Go lets a function ask, and a flag set
// while a command runs would make an outside Close during a command skip the
// very wait this exists for.
func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		if s.http != nil {
			s.http.Close()
		}
		close(s.done)
		if s.stopped != nil {
			<-s.stopped
		}
		err = s.writer.Close()
	})
	return err
}

// loop is the only goroutine that touches the session.
func (s *Server) loop() {
	defer close(s.stopped)
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
// errServerClosing is a request that did not reach the loop because the process
// is shutting down. It is not the session needing recovery: that one is a
// journal to inspect, and this one is a server to start again.
var errServerClosing = errors.New("ui: the server is closing")

func (s *Server) ask(run func()) error {
	finished := make(chan struct{})
	select {
	case s.commands <- func() { run(); close(finished) }:
		<-finished
		return nil
	case <-s.done:
		return errServerClosing
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
			writeJSON(w, http.StatusForbidden, refusal{ReasonForeignOrigin,
				"ui: this server answers only its own origin"})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/state" && r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, refusal{ReasonMethodNotAllowed,
				"ui: commands are POST"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	var state State
	if err := s.ask(func() { state = s.state() }); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, refusal{ReasonServerClosing, err.Error()})
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
		writeJSON(w, http.StatusServiceUnavailable, refusal{ReasonServerClosing, err.Error()})
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
		// An absent or unreadable body presents no key, and is refused as one —
		// but one past the ceiling is refused as what it is before it is read.
		if _, ok := s.decodeBody(w, r, &body); !ok {
			return
		}
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
		s.refuse(w, err)
		return
	}
	if err != nil {
		// The only 500 this server has: minting a lease needs the machine's
		// randomness, and nothing else here can fail for a reason that is
		// neither the client's nor the session's.
		writeJSON(w, http.StatusInternalServerError, refusal{ReasonLeaseNotMinted, err.Error()})
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

// halted is the failure that stopped this session, wrapped so that it classifies
// as the refusal the kernel gives for the same fact, or nil.
//
// It is asked at the top of every turn that would otherwise answer out of this
// adapter's own memory, and that ordering is the whole of it. The gesture
// register and the step record are written while a command mutates the
// aggregates, which ADR-012 puts before the commit is attempted; a command
// whose commit then failed leaves both of them describing an act the store
// never took. The shortcuts that read them — this gesture is a retry, this
// gesture is a reuse, this step is stale — are answered without the kernel
// being asked at all, so the kernel's own refusal, correct as it is, is never
// reached.
//
// It is asked before the lease, which is deliberate. A dead session and a lease
// somebody else now holds are both true, and only one of them is worth acting
// on: told the lease is stale, a client takes the controls again and goes on
// trading a run that has stopped. State reports the two in the same order and
// for the same reason.
func (s *Server) halted() error {
	err := s.session.NeedsRecovery()
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", session.ErrSessionNeedsRecovery, err)
}

// maxBodyBytes is what a request may weigh. Every body this protocol has is a
// handful of decimal strings, so the ceiling is generous by three orders of
// magnitude and still bounds what one page open in a browser can make this
// process hold.
const maxBodyBytes = 64 << 10

// decodeBody reads a request body under that ceiling, and answers the two ways
// it can fail as the two findings they are: bytes that cannot be read, and bytes
// that can and are too many. It reports whether the caller may go on, and
// whether anything was decoded at all — an empty body is not a failure for a
// route whose body is optional.
func (s *Server) decodeBody(w http.ResponseWriter, r *http.Request, into any) (decoded, ok bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	err := json.NewDecoder(r.Body).Decode(into)
	switch {
	case err == nil:
		return true, true
	case errors.Is(err, io.EOF):
		return false, true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeJSON(w, http.StatusRequestEntityTooLarge, refusal{ReasonBodyTooLarge,
			fmt.Sprintf("ui: a request body may weigh %d bytes", maxBodyBytes)})
		return false, false
	}
	writeJSON(w, http.StatusBadRequest, refusal{ReasonUnreadable, err.Error()})
	return false, false
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

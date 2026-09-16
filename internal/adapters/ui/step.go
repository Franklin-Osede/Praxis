package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"praxis/internal/adapters/marketdata"
	"praxis/internal/market"
	"praxis/internal/session"
)

// errStaleStep is a step that advances from a row the screen no longer stands
// on and that is not a retry of the last step: a stale tab, or a duplicate so
// late another step has happened since.
var errStaleStep = errors.New("ui: this step advances from a row that is no longer on the screen")

// advance is what a client sends to move the market one row on.
//
// It carries no gesture, because a step is not a decision the journal records
// as one: the rows it produces are recorded, and the interval a person took
// before asking for the next one is the gap between two presentations, which
// section 11 calls the record. So a step is idempotent by content, like the
// acknowledgement and unlike the four commands, and its content is the row it
// advances from.
type advance struct {
	Lease string `json:"lease"`

	// FromObservedSequence is the observation on the screen when the person
	// asked, as the state named it. Absent is the empty string, for a session
	// nothing has been shown in yet.
	FromObservedSequence string `json:"fromObservedSequence"`
}

// stepRecord is the last step this server applied: the run it was taken in, the
// row it advanced from, and the row it put on the screen. It is what tells a
// retry from a stale step, and it is kept here rather than derived from the
// journal because a retry only means something inside the lease that sent it —
// a restart grants a new lease, and a request from the old one is refused before
// this is consulted.
type stepRecord struct {
	segment, from, to uint64
}

// handleStep moves the market one row on for the person holding the controls.
//
// Everything after reading the body happens inside one turn of the loop, for the
// reason every command does: the lease, the row on screen, and the step have to
// be one reading, or a transfer or a second tab could land between them.
func (s *Server) handleStep(w http.ResponseWriter, r *http.Request) {
	var body advance
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, refusal{ReasonUnreadable, err.Error()})
		return
	}
	from, err := optionalSequence(body.FromObservedSequence)
	if err != nil {
		s.refuse(w, err)
		return
	}

	var (
		state     State
		refusedBy error
	)
	if err := s.ask(func() {
		segment, err := s.lease.check(body.Lease)
		if err != nil {
			refusedBy = err
			return
		}
		refusedBy = s.step(segment, from)
		state = s.state()
	}); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, refusal{ReasonServerClosing, err.Error()})
		return
	}
	if refusedBy != nil {
		s.refuse(w, refusedBy)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// step is the rule, on the loop. Its order is a decision, and this is it:
//
//  1. A step from a row that is not on the screen is a retry if the last step in
//     this run started from that row and nothing has moved since; it is
//     answered with the state and applies nothing. Otherwise it is stale.
//  2. The row on the screen must have been confirmed in this run before the
//     market moves past it. Section 11 makes the gap between two presentations
//     the record, and a row that passed with no presentation would be a hole in
//     it. A session that has shown nothing has nothing to confirm.
//  3. Only then is a row applied, and the file running out or the evaluation
//     having ended are refused there, before anything is written.
//
// The retry is answered before the confirmation is asked for, deliberately, the
// order the acknowledgement answers its own retry in. A lost response leaves a
// row on the screen that the client never saw and so never confirmed; asking
// first would turn the loss into a refusal for a step the person already took.
// And the confirmation is asked before the end of the file, because it is about
// what is on the screen, whatever does or does not lie after it.
func (s *Server) step(segment, from uint64) error {
	current := s.session.LastObserved()
	if from != current {
		last := s.lastStep
		if last.segment == segment && last.from == from && last.to == current {
			return nil
		}
		return fmt.Errorf("%w: it advances from observation %d, and the screen stands on %d",
			errStaleStep, from, current)
	}
	if current != 0 && !s.session.Presented(segment) {
		return fmt.Errorf("%w: advancing past observation %d, which segment %d has not confirmed",
			session.ErrNotYetPresented, current, segment)
	}

	next, err := marketdata.Step(s.session, s.feed, s.cursor)
	// Step returns the row to apply next even when it refuses, and a step whose
	// boundary committed before its observation failed is re-entrant from the
	// same row. The cursor follows what Step says rather than what was hoped.
	s.cursor = next
	if err != nil {
		return err
	}
	s.lastStep = stepRecord{segment: segment, from: from, to: s.session.LastObserved()}
	return nil
}

// optionalSequence reads an observation a step may leave out. Absent is the
// empty string, as it is for an optional price. No observation has sequence
// zero, so a zero is refused rather than read as "none": accepting it would give
// "nothing on the screen" two spellings, and a client would retry or advance
// depending on which one it happened to send.
func optionalSequence(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	v, err := market.ParseUint(s)
	if err != nil {
		return 0, err
	}
	if v == 0 {
		return 0, fmt.Errorf("%w: %q names no observation; nothing on the screen is an absent field",
			market.ErrNotCanonicalInt, s)
	}
	return v, nil
}

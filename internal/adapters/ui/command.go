package ui

import (
	"encoding/json"
	"errors"
	"net/http"

	"praxis/internal/market"
	"praxis/internal/session"
)

// Reason names why a request was refused, from a closed set.
//
// The status is a coarse bucket — there are several distinct 409s — so a client
// that discriminated on it could not. The reason is what it reads, and it is
// enumerated here and in the specification rather than left to be matched as a
// substring of a sentence: a sentence is prose and will be improved, and a
// client matching on prose breaks when it is.
type Reason string

const (
	// 400: the bytes are wrong.
	ReasonUnreadable        Reason = "unreadable"
	ReasonNotCanonical      Reason = "not_canonical"
	ReasonInvalidIdentifier Reason = "invalid_identifier"

	// 409: something is already taken.
	ReasonLeaseHeld       Reason = "lease_held"
	ReasonLeaseStale      Reason = "lease_stale"
	ReasonGestureConflict Reason = "gesture_conflict"
	ReasonWrongRun        Reason = "wrong_run"

	// 422: the session refuses this command now.
	ReasonOutOfOrder      Reason = "out_of_order"
	ReasonNotPresented    Reason = "not_presented"
	ReasonEvaluationEnded Reason = "evaluation_ended"
	ReasonNoSessionOpen   Reason = "no_session_open"
	ReasonRefused         Reason = "refused"

	// 503: the session has stopped.
	ReasonNeedsRecovery Reason = "needs_recovery"
)

// errUnknownKind and errWrongSegment are this adapter's own refusals: a tag it
// does not know, and an act naming a run it does not belong to.
var (
	errUnknownKind  = errors.New("ui: the body does not name something this accepts")
	errWrongSegment = errors.New("ui: this act names a run it does not belong to")
)

// refusal is the body of every response that is not a 200.
type refusal struct {
	Reason Reason `json:"reason"`
	Detail string `json:"detail"`
}

// classify turns a refusal from the session into the status and reason a client
// reads. It is one place, so the four commands cannot answer differently for
// the same cause.
//
// It reads sentinels and never a sentence, which is what the readers' switch to
// %w bought: before it, every refusal from Replay or Verify arrived as one
// wrapper and the only way to tell them apart was the text.
func classify(err error) (int, refusal) {
	switch {
	case errors.Is(err, session.ErrSessionNeedsRecovery):
		return http.StatusServiceUnavailable, refusal{ReasonNeedsRecovery, err.Error()}
	case errors.Is(err, ErrControllerActive):
		return http.StatusConflict, refusal{ReasonLeaseHeld, err.Error()}
	case errors.Is(err, ErrStaleLease):
		return http.StatusConflict, refusal{ReasonLeaseStale, err.Error()}
	case errors.Is(err, session.ErrGestureReused):
		return http.StatusConflict, refusal{ReasonGestureConflict, err.Error()}
	case errors.Is(err, errWrongSegment):
		return http.StatusConflict, refusal{ReasonWrongRun, err.Error()}
	case errors.Is(err, errUnknownKind):
		return http.StatusBadRequest, refusal{ReasonUnreadable, err.Error()}
	case errors.Is(err, market.ErrNotCanonicalInt):
		return http.StatusBadRequest, refusal{ReasonNotCanonical, err.Error()}
	case errors.Is(err, market.ErrIdentifierCharacter), errors.Is(err, market.ErrEmptyOrderID):
		return http.StatusBadRequest, refusal{ReasonInvalidIdentifier, err.Error()}
	case errors.Is(err, session.ErrInteractionOrder), errors.Is(err, session.ErrMalformedDecision):
		return http.StatusUnprocessableEntity, refusal{ReasonOutOfOrder, err.Error()}
	case errors.Is(err, session.ErrNotYetPresented), errors.Is(err, session.ErrNothingToPresent),
		errors.Is(err, session.ErrWrongPresentation):
		return http.StatusUnprocessableEntity, refusal{ReasonNotPresented, err.Error()}
	case errors.Is(err, session.ErrChallengeEnded):
		return http.StatusUnprocessableEntity, refusal{ReasonEvaluationEnded, err.Error()}
	case errors.Is(err, session.ErrNoSessionOpen):
		return http.StatusUnprocessableEntity, refusal{ReasonNoSessionOpen, err.Error()}
	default:
		return http.StatusUnprocessableEntity, refusal{ReasonRefused, err.Error()}
	}
}

func (s *Server) refuse(w http.ResponseWriter, err error) {
	code, body := classify(err)
	writeJSON(w, code, body)
}

// acknowledgement is what a client sends to confirm an observation reached the
// screen. It carries no gesture: its identity is its content — the segment and
// the observation — so it has no "same name, different command" case to have,
// which is the whole reason the other four need a name and this one does not.
type acknowledgement struct {
	Lease            string `json:"lease"`
	ObservedSequence string `json:"observedSequence"`
	AtUTCNanos       string `json:"atUtcNanos"`
	ElapsedNanos     string `json:"elapsedNanos"`
}

// handleAcknowledge records that an observation was put in front of a person.
//
// Everything after reading the body happens inside one turn of the loop: the
// lease is checked there and not before it, so a transfer cannot land between
// the check and the act, and the refusal names the lease rather than reporting
// a reading out of order to someone whose controls were taken away.
func (s *Server) handleAcknowledge(w http.ResponseWriter, r *http.Request) {
	var body acknowledgement
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, refusal{ReasonUnreadable, err.Error()})
		return
	}
	observed, err := market.ParseUint(body.ObservedSequence)
	if err != nil {
		s.refuse(w, err)
		return
	}
	atUTC, err := market.ParseInt(body.AtUTCNanos)
	if err != nil {
		s.refuse(w, err)
		return
	}
	elapsed, err := market.ParseInt(body.ElapsedNanos)
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
		// The presentation's identity is the segment it was shown in and the
		// observation it showed. A repeat records nothing and is not an error.
		refusedBy = s.session.AcknowledgePresentation(
			session.PresentationID{Segment: segment, ObservedSequence: observed},
			session.Instant{
				AtUTCNanos:   session.UnixNanos(atUTC),
				Segment:      segment,
				ElapsedNanos: session.ElapsedNanos(elapsed),
			})
		state = s.state()
	}); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, refusal{ReasonNeedsRecovery, err.Error()})
		return
	}
	if refusedBy != nil {
		s.refuse(w, refusedBy)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

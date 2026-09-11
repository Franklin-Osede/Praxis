package ui

import "time"

// Reading is the two facts a stamp is made of, taken together.
//
// They are separate because they answer different questions and fail in
// different ways. Wall says when in the world something happened and is for
// audit; it can legitimately move backwards, because a time server corrects it,
// an operator sets it, or a machine resumes from suspension. Mono only ever
// goes forward, and is the only thing an interval is computed from.
//
// Instant models them apart for the same reason. A clock that handed back one
// time.Time collapsed them, and whoever received it had to split them again —
// which is where a wall reading ends up inside a measurement by accident.
type Reading struct {
	Wall time.Time

	// Mono is a count since an origin this clock chose. Only differences
	// between two of its own readings mean anything; the origin itself does
	// not, and nothing records it.
	Mono time.Duration
}

// Clock is where an adapter's readings come from. Rule 2 forbids one in the
// domain and allows one here; this is injected rather than called so that the
// same acts produce the same journal twice, which is what makes a run through
// the interface comparable byte for byte with one taken directly.
type Clock func() Reading

// SystemClock reads the machine. Its monotonic count comes from subtracting two
// time.Now values, which uses their monotonic readings and is therefore immune
// to the wall clock being set.
//
// This is where injection stops, and it is the one place in this file no test
// reaches: replacing the Sub below with UnixNano arithmetic passes everything,
// because verifying it would mean setting the machine's clock backwards. That
// is not a gap left open so much as where the boundary is — the only clock that
// is not injected cannot be checked by injecting a clock. It is four lines with
// no branches, and everything that computes anything from it is injected and
// tested.
func SystemClock() Clock {
	origin := time.Now()
	return func() Reading {
		now := time.Now()
		return Reading{Wall: now, Mono: now.Sub(origin)}
	}
}

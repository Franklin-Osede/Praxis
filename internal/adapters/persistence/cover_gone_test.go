package persistence_test

import (
	"errors"
	"strings"
	"testing"

	"praxis/internal/adapters/persistence"
	"praxis/internal/session"
)

// coverGone is the ending a protection gets when its last leg is cancelled
// over exposure that is still open.
func coverGone() []session.Event {
	events := everyEventTypeV5()
	for n, e := range events {
		if ended, ok := e.(session.ProtectionEnded); ok {
			ended.Reason = session.ProtectionCoverGone
			events[n] = ended
		}
	}
	return events
}

// Scenario: a version with no name for an ending refuses to write it
//
// The rule cancelReasonsSince already holds for cancellations: a writer that
// produced a name its reader has never heard of would write a line that reader
// rejects, which is worse than refusing at the door.
func TestAnEndingV5HasNoNameForIsRefused(t *testing.T) {
	for _, version := range []string{
		persistence.EventVersionV3, persistence.EventVersionV4, persistence.EventVersionV5,
	} {
		if _, err := persistence.EncodeEvents(coverGone(), version); !errors.Is(err, persistence.ErrUnsupportedInVersion) {
			t.Fatalf("%s encoded an ending it has no name for: %v", version, err)
		}
	}
}

// Scenario: the version that published it writes and reads it back
func TestV6CarriesTheEndingItPublished(t *testing.T) {
	payload, err := persistence.EncodeEvents(coverGone(), persistence.EventVersionV6)
	if err != nil {
		t.Fatalf("EncodeEvents: %v", err)
	}
	if !strings.Contains(string(payload), "cover_gone") {
		t.Fatal("the payload does not name the ending")
	}
	events, err := persistence.DecodeEvents(payload, persistence.EventVersionV6)
	if err != nil {
		t.Fatalf("DecodeEvents: %v", err)
	}
	var found bool
	for _, e := range events {
		if ended, ok := e.(session.ProtectionEnded); ok && ended.Reason == session.ProtectionCoverGone {
			found = true
		}
	}
	if !found {
		t.Fatal("the ending did not survive the round trip")
	}
}

// Scenario: a decoder is not told to believe a name its version cannot hold
//
// The gate is on both sides, because a decoder must not trust the bytes it
// reads: a v5 journal naming cover_gone was not written by this system.
func TestADecoderRefusesAnEndingItsVersionCannotHold(t *testing.T) {
	payload, err := persistence.EncodeEvents(coverGone(), persistence.EventVersionV6)
	if err != nil {
		t.Fatalf("EncodeEvents: %v", err)
	}
	if _, err := persistence.DecodeEvents(payload, persistence.EventVersionV5); !errors.Is(err, persistence.ErrUnsupportedInVersion) {
		t.Fatalf("v5 decoded a v6 ending: %v", err)
	}
}

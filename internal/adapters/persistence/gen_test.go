package persistence_test

import (
	"os"
	"testing"

	"praxis/internal/adapters/persistence"
	"praxis/internal/session"
)

func TestGenerateGolden(t *testing.T) {
	if os.Getenv("PRAXIS_GENERATE_GOLDEN") == "" {
		t.Skip("generation is not part of the suite")
	}
	for _, g := range []struct {
		version string
		events  []session.Event
		path    string
	}{
		{persistence.EventVersionV1, everyEventType(), "testdata/golden-events.txt"},
		{persistence.EventVersionV2, everyEventTypeV2(), "testdata/golden-events-v2.txt"},
		{persistence.EventVersionV3, everyEventTypeV3(), "testdata/golden-events-v3.txt"},
		{persistence.EventVersionV4, everyEventTypeV4(), "testdata/golden-events-v4.txt"},
	} {
		payload, err := persistence.EncodeEvents(g.events, g.version)
		if err != nil {
			t.Fatalf("EncodeEvents %s: %v", g.version, err)
		}
		if err := os.WriteFile(g.path, payload, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
}

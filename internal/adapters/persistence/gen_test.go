package persistence_test

import (
	"os"
	"testing"

	"praxis/internal/adapters/persistence"
)

func TestGenerateGolden(t *testing.T) {
	if os.Getenv("PRAXIS_GENERATE_GOLDEN") == "" {
		t.Skip("generation is not part of the suite")
	}
	payload, err := persistence.EncodeEvents(everyEventType())
	if err != nil {
		t.Fatalf("EncodeEvents: %v", err)
	}
	if err := os.WriteFile("testdata/golden-events.txt", payload, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

package persistence_test

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// Scenario: a golden cannot be regenerated without saying so twice
//
// A golden is only as strong as the rule that it is never quietly rewritten,
// and that rule was broken once: SourceSequence was inserted into v1's
// market_observed line and the golden was updated to match, so the bytes v1
// first froze no longer decode.
//
// The failure mode is not malice. It is a reflex — the golden goes red, the
// generator is there, and regenerating it is one command. So changing a golden
// now means changing two files, and the second is a list of checksums that no
// reviewer can skim past.
func TestTheFrozenPayloadsAreStillFrozen(t *testing.T) {
	const manifest = "testdata/goldens.sha256"

	f, err := os.Open(manifest)
	if err != nil {
		t.Fatalf("open %s: %v", manifest, err)
	}
	defer f.Close()

	listed := map[string]bool{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		want, name, found := strings.Cut(line, "  ")
		if !found {
			t.Fatalf("%s: line %q is not \"<sha256>  <file>\"", manifest, line)
		}
		listed[name] = true

		payload, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		sum := sha256.Sum256(payload)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Fatalf("%s changed.\n got: %s\nwant: %s\n\n"+
				"If this is a new payload version, it needs a new file and a new line here.\n"+
				"If it is a change to bytes a journal already holds, it is not allowed:\n"+
				"see the version rule at the top of codec.go.", name, got, want)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read %s: %v", manifest, err)
	}

	// Every golden is listed. A new one that nobody adds to the manifest would
	// otherwise be frozen by a file that does not mention it.
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "golden-") || !listed[name] {
			if strings.HasPrefix(name, "golden-") {
				t.Fatalf("%s is not listed in %s", name, manifest)
			}
		}
	}
	if len(listed) == 0 {
		t.Fatalf("%s lists nothing", manifest)
	}
}

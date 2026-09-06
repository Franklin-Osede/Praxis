package persistence_test

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"praxis/internal/adapters/persistence"
	"praxis/internal/market"
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

// Scenario: the domain and the format agree on what a name may contain
//
// The domain refuses names the record cannot hold, so that a command which
// could not be written down is never accepted. The format refuses them again,
// because a decoder must not trust the bytes it is reading. Two implementations
// of one rule is exactly the arrangement this project distrusts, so they are
// held to the same set here rather than assumed to agree.
func TestTheDomainAndTheFormatAgreeOnIdentifiers(t *testing.T) {
	// Every rune the format could plausibly meet, plus the ones that broke it.
	var candidates []rune
	for r := rune(0); r < 128; r++ {
		candidates = append(candidates, r)
	}
	candidates = append(candidates, 'ó', 'ñ', '€', '中', ' ')

	for _, r := range candidates {
		name := "a" + string(r) + "b"
		domain := market.ValidIdentifier(name) == nil
		format := persistence.ValidIdentifierForTest(name) == nil
		if domain != format {
			t.Fatalf("%q: the domain says %v and the format says %v", name, domain, format)
		}
	}

	// And both refuse an empty name, which is not a character question.
	if (market.ValidIdentifier("") == nil) != (persistence.ValidIdentifierForTest("") == nil) {
		t.Fatal("the two disagree about an empty name")
	}
}

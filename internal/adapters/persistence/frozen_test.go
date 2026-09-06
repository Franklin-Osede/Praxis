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
	"praxis/internal/session"
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

// Scenario: a journal's configuration has a digest a pre-registration can hold
//
// Everything a journal proves, it proves relative to its configuration: the
// commission a fill was charged, the balance it started from, the rules it is
// judged against and who traded it are axioms, not conclusions. Nothing inside
// can catch a journal run with the wrong ones, because everything downstream is
// consistent with whatever they were — a session run at zero commission loses
// five hundred cents of fees and stays perfectly self-consistent.
//
// The answer is not a check inside the journal. It is that the digest is
// recorded before any session is traded, and confirmed from outside afterwards.
func TestAConfigurationHasADigestThatMovesWhenItDoes(t *testing.T) {
	started := func(commission market.Cents, subject string) session.SessionStarted {
		return session.SessionStarted{
			Envelope: session.Envelope{Time: 1_000, Sequence: 1, Kind: session.KindSessionStarted},
			Config: session.Config{
				Instrument: mnq, SubjectID: subject,
				StartingBalanceCts: 5_000_000, CommissionPerContractCts: commission,
			},
		}
	}

	base, err := persistence.ConfigDigest(started(50, "t-01"), persistence.EventVersion)
	if err != nil {
		t.Fatalf("ConfigDigest: %v", err)
	}
	again, err := persistence.ConfigDigest(started(50, "t-01"), persistence.EventVersion)
	if err != nil {
		t.Fatalf("ConfigDigest: %v", err)
	}
	if base != again || len(base) != 64 {
		t.Fatalf("digest is not a stable sha256: %q then %q", base, again)
	}

	// Every axiom it covers moves it. The commission one is the case that
	// motivated it: a cheaper journal is entirely coherent and entirely wrong.
	for _, other := range []session.SessionStarted{
		started(0, "t-01"),
		started(50, "t-02"),
		started(50, ""),
	} {
		got, err := persistence.ConfigDigest(other, persistence.EventVersion)
		if err != nil {
			t.Fatalf("ConfigDigest: %v", err)
		}
		if got == base {
			t.Fatalf("a different configuration has the same digest: %+v", other.Config)
		}
	}
}

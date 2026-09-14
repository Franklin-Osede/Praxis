package persistence_test

import (
	"errors"
	"strings"
	"testing"

	"praxis/internal/adapters/persistence"
)

// An anchor is compared by equality, so its spelling decides whether two equal
// anchors look equal. These tests hold the same discipline the format already
// applies to a batch header: one value, exactly one encoding, and a reader that
// refuses anything else rather than accepting a second spelling.

const goodDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func sound() persistence.Anchor {
	return persistence.Anchor{LastBatch: 5, LastSequence: 11, PrefixDigest: goodDigest}
}

func formatted(t *testing.T, a persistence.Anchor) string {
	t.Helper()
	s, err := a.Format()
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	return s
}

// Scenario: writing an anchor and reading it back returns the same anchor
func TestAnchorFormatAndParseAreInverses(t *testing.T) {
	for name, a := range map[string]persistence.Anchor{
		"no run named": sound(),
		"a run named":  {RunID: "run-01", LastBatch: 5, LastSequence: 11, PrefixDigest: goodDigest},
		"first batch":  {LastBatch: 1, LastSequence: 1, PrefixDigest: goodDigest},
	} {
		t.Run(name, func(t *testing.T) {
			text := formatted(t, a)
			back, err := persistence.ParseAnchor(text)
			if err != nil {
				t.Fatalf("ParseAnchor(%q): %v", text, err)
			}
			if back != a {
				t.Fatalf("round trip changed the anchor:\n in:  %+v\n out: %+v", a, back)
			}
			// And the other way, because an inverse is two claims and only one
			// of them is that parsing undoes formatting.
			if again := formatted(t, back); again != text {
				t.Fatalf("re-formatting changed the text:\n %q\n %q", text, again)
			}
		})
	}
}

// Scenario: an anchor taken from a real journal survives being written down
//
// The round trip above uses literals. This one closes the loop the CLI will
// actually walk: take an anchor, write it, read it back, and check the journal
// with what came back rather than with what was produced.
func TestAnAnchorSurvivesBeingWrittenDownAndReadBack(t *testing.T) {
	path, _ := twoBatches(t)
	taken := anchorOf(t, path)

	back, err := persistence.ParseAnchor(formatted(t, taken))
	if err != nil {
		t.Fatalf("ParseAnchor: %v", err)
	}
	if back != taken {
		t.Fatalf("the journal's anchor did not survive a round trip:\n %+v\n %+v", taken, back)
	}
	if err := persistence.CheckAnchor(path, back); err != nil {
		t.Fatalf("CheckAnchor with a parsed anchor: %v", err)
	}
}

// Scenario: an anchor with no run named is spelled the way the format spells an
// absent identifier
//
// "-" is the codec's existing spelling, not a second one invented here. An
// empty field would also be unparseable, since the fields are space separated.
func TestAnAbsentRunIsSpelledAsTheFormatSpellsOne(t *testing.T) {
	text := formatted(t, sound())
	if fields := strings.Split(text, " "); fields[1] != "-" {
		t.Fatalf("an absent run is spelled %q, want %q", fields[1], "-")
	}
	if !strings.HasPrefix(text, "praxis.anchor.v1 ") {
		t.Fatalf("anchor text does not name its version: %q", text)
	}
}

// Scenario: a malformed anchor has no written form
//
// Format refuses rather than producing text, on the rule this format already
// applies to a name it cannot hold. Evidence that cannot be checked must not
// acquire a spelling that looks authoritative.
func TestAMalformedAnchorCannotBeWrittenDown(t *testing.T) {
	if _, err := (persistence.Anchor{}).Format(); !errors.Is(err, persistence.ErrAnchorMalformed) {
		t.Fatalf("Format of a zero anchor: got %v, want ErrAnchorMalformed", err)
	}
}

// Scenario: every spelling that is not the spelling is refused
func TestParseAnchorRefusesEverySecondSpelling(t *testing.T) {
	good := formatted(t, sound())

	for name, text := range map[string]string{
		"empty":                "",
		"no version":           "run-01 5 11 sha256:" + goodDigest,
		"wrong version":        "praxis.anchor.v2 - 5 11 sha256:" + goodDigest,
		"too few fields":       "praxis.anchor.v1 - 5 sha256:" + goodDigest,
		"too many fields":      good + " extra",
		"leading zero batch":   "praxis.anchor.v1 - 05 11 sha256:" + goodDigest,
		"leading zero seq":     "praxis.anchor.v1 - 5 011 sha256:" + goodDigest,
		"batch not a number":   "praxis.anchor.v1 - five 11 sha256:" + goodDigest,
		"batch zero":           "praxis.anchor.v1 - 0 11 sha256:" + goodDigest,
		"sequence zero":        "praxis.anchor.v1 - 5 0 sha256:" + goodDigest,
		"no digest algorithm":  "praxis.anchor.v1 - 5 11 " + goodDigest,
		"wrong algorithm":      "praxis.anchor.v1 - 5 11 sha1:" + goodDigest,
		"short digest":         "praxis.anchor.v1 - 5 11 sha256:abcdef",
		"upper-case digest":    "praxis.anchor.v1 - 5 11 sha256:" + strings.ToUpper(goodDigest),
		"digest not hex":       "praxis.anchor.v1 - 5 11 sha256:" + goodDigest[:63] + "z",
		"empty run field":      "praxis.anchor.v1  5 11 sha256:" + goodDigest,
		"run outside alphabet": "praxis.anchor.v1 run/01 5 11 sha256:" + goodDigest,
		"leading space":        " " + good,
		"trailing space":       good + " ",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := persistence.ParseAnchor(text); !errors.Is(err, persistence.ErrAnchorMalformed) {
				t.Fatalf("ParseAnchor(%q): got %v, want ErrAnchorMalformed", text, err)
			}
		})
	}
}

// Scenario: the good spelling is not accidentally refused
//
// A rejection table proves nothing on its own — a parser that refused
// everything would pass all of it.
func TestParseAnchorAcceptsTheSpellingItProduces(t *testing.T) {
	if _, err := persistence.ParseAnchor(formatted(t, sound())); err != nil {
		t.Fatalf("the parser refused its own output: %v", err)
	}
}

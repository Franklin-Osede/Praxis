package persistence_test

import (
	"errors"
	"strings"
	"testing"

	"praxis/internal/adapters/persistence"
	"praxis/internal/market"
)

// A decoder does not trust the bytes it reads. That was already the rule for the
// shape of a line — a different spelling of a number, an unknown enumeration, a
// missing field — and it stopped there: bytes that parsed perfectly and
// described something the domain calls impossible were rebuilt into a typed
// value and handed on. The live path refuses those at the door; the read path
// did not, which is the writer/reader asymmetry this repository has been bitten
// by twice, in the values nothing downstream recomputes.
//
// The accusation is its own, because it is its own finding: ErrSyntax says these
// bytes are not canonical text, and ErrNotADomainValue says they are, and do not
// describe a value. The domain's own error is wrapped inside, so a reader can ask
// which rule was broken.

// goldenLine is one line of the v4 golden, so every case starts from bytes this
// codec really wrote.
func goldenLine(t *testing.T, kind string) []string {
	t.Helper()
	for _, line := range strings.Split(string(goldenFor(t, "testdata/golden-events-v4.txt")), "\n") {
		if strings.HasPrefix(line, kind+" ") {
			return strings.Split(line, " ")
		}
	}
	t.Fatalf("the golden holds no %s line", kind)
	return nil
}

// rewritten is that line with one field replaced.
func rewritten(t *testing.T, kind string, field int, value string) []byte {
	t.Helper()
	fields := goldenLine(t, kind)
	if field >= len(fields) {
		t.Fatalf("%s has %d fields, cannot rewrite %d", kind, len(fields), field)
	}
	fields[field] = value
	return []byte(strings.Join(fields, " ") + "\n")
}

// Scenario: bytes that parse and are not a domain value are refused
func TestTheDecoderRefusesAValueTheDomainCallsImpossible(t *testing.T) {
	for name, tc := range map[string]struct {
		kind  string
		field int
		value string
		says  error
	}{
		// A bid above the ask: the one state Quote.Validate exists to call
		// invalid input rather than a tradable book.
		"a crossed quote": {"market_observed", 7, "20002", market.ErrCrossedQuote},

		// An instrument whose tick is worth nothing, which would make every
		// amount computed from it zero.
		"an instrument with no tick value": {"market_observed", 4, "0", market.ErrNonPositiveTickValue},

		// A market order still carrying the limit price of the order it used to
		// be. Zero means unset, so a price left behind is not ignorable.
		"a market order with a limit price": {"order_submitted", 7, "market", market.ErrUnexpectedLimitPrice},

		// An order waiting for nothing.
		"a resting order of no quantity": {"order_rested", 8, "0", market.ErrNonPositiveQty},

		// A fill that filled nothing.
		"a fill of no quantity": {"fill_produced", 9, "0", market.ErrNonPositiveQty},
	} {
		t.Run(name, func(t *testing.T) {
			payload := rewritten(t, tc.kind, tc.field, tc.value)

			_, err := persistence.DecodeEvents(payload, persistence.EventVersionV4)
			if !errors.Is(err, persistence.ErrNotADomainValue) {
				t.Fatalf("DecodeEvents: got %v, want ErrNotADomainValue", err)
			}
			// And the domain's own refusal is reachable through it, so a reader
			// learns which rule was broken rather than only that one was.
			if !errors.Is(err, tc.says) {
				t.Fatalf("the refusal does not name %v: %v", tc.says, err)
			}
			// It is not the syntax finding: these bytes are canonical text.
			if errors.Is(err, persistence.ErrSyntax) {
				t.Fatalf("a well formed line was reported as bad syntax: %v", err)
			}
		})
	}
}

// Scenario: the line each case starts from decodes when it is left alone
//
// Without this, a case could pass because the line was refused for some other
// reason and the rewriting changed nothing.
func TestTheLinesTheseCasesRewriteDecodeUntouched(t *testing.T) {
	for _, kind := range []string{"market_observed", "order_submitted", "order_rested", "fill_produced"} {
		t.Run(kind, func(t *testing.T) {
			untouched := []byte(strings.Join(goldenLine(t, kind), " ") + "\n")
			if _, err := persistence.DecodeEvents(untouched, persistence.EventVersionV4); err != nil {
				t.Fatalf("the untouched %s line does not decode: %v", kind, err)
			}
		})
	}
}

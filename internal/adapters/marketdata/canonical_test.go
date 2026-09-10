package marketdata_test

import (
	"errors"
	"strings"
	"testing"

	"praxis/internal/adapters/marketdata"
	"praxis/internal/market"
)

// Scenario: the market file is held to the journal's spelling of a number
//
//	Given a row whose figures are written in a form the journal would refuse
//	Then the file is refused too.
//
// A market file is the input everything else rests on, and two spellings of the
// same number are two files that mean the same thing — which is the property
// that makes comparing a journal to its file worth doing at all. It was the one
// boundary parsing with a bare strconv: `+20000`, `020000` and `-0` were
// accepted here and refused by the journal, and nothing was deciding that, it
// was simply unlooked at.
func TestTheMarketFileIsHeldToTheCanonicalSpelling(t *testing.T) {
	for _, tc := range []struct{ name, row string }{
		{"a sign on a positive price", "3000,1,d1,+20000,20001,10,10"},
		{"a leading zero on a price", "3000,1,d1,020000,20001,10,10"},
		{"a negative zero size", "3000,1,d1,20000,20001,-0,10"},
		{"a sign on the sequence", "3000,1,d1,20000,20001,10,+10"},
		{"a leading zero on the time", "03000,1,d1,20000,20001,10,10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := marketdata.Read(strings.NewReader(header + tc.row + "\n"))
			if err == nil {
				t.Fatal("the file was accepted")
			}
			if !errors.Is(err, marketdata.ErrField) && !errors.Is(err, market.ErrNotCanonicalInt) {
				t.Fatalf("refused for another reason: %v", err)
			}
		})
	}

	t.Run("and the canonical spelling still reads", func(t *testing.T) {
		feed, err := marketdata.Read(strings.NewReader(header + "3000,1,d1,20000,20001,10,10\n"))
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if len(feed.Observations) != 1 {
			t.Fatalf("observations: got %d, want 1", len(feed.Observations))
		}
	})

	t.Run("the header's tick value too", func(t *testing.T) {
		bad := strings.Replace(header, ",50\n", ",050\n", 1)
		if bad == header {
			t.Fatal("the fixture did not change, so this proves nothing")
		}
		if _, err := marketdata.Read(strings.NewReader(bad + "3000,1,d1,20000,20001,10,10\n")); err == nil {
			t.Fatal("a header with a leading zero on the tick value was accepted")
		}
	})
}

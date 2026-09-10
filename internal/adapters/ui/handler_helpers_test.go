package ui_test

import (
	"fmt"
	"strconv"
)

func fmtSscan(s string, v *uint64) (int, error) { return fmt.Sscan(s, v) }

// gestureName is what a client mints: the run the server issued, and its own
// counter within it.
func gestureName(segment uint64, sequence int) string {
	return strconv.FormatUint(segment, 10) + ":" + strconv.Itoa(sequence)
}

package session

import "praxis/internal/market"

// ConsumeBookForTest exposes the subtraction so a test can reach its guard
// directly. Nothing a live session does can drive a book negative, so the guard
// is otherwise only reachable through a forged journal, where an earlier check
// gets there first.
func ConsumeBookForTest(q market.Quote, fills []market.Fill) (market.Quote, error) {
	return consumeBook(q, fills)
}

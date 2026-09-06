package persistence

// ValidIdentifierForTest exposes the format's own gate, so a test can hold it
// to the same set as the domain's rather than assume the two agree.
func ValidIdentifierForTest(s string) error { return validIdentifier(s) }

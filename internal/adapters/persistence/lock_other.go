//go:build !unix

package persistence

import "os"

// lockExclusive is unavailable off POSIX. The store refuses to open rather
// than run without the exclusion it promises.
func lockExclusive(f *os.File) error { return ErrLockUnsupported }

func lockShared(f *os.File) error { return ErrLockUnsupported }

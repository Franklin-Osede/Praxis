//go:build unix

package persistence

import (
	"errors"
	"os"
	"syscall"
)

// lockExclusive takes an advisory exclusive lock on the open file, without
// waiting. The kernel releases it when the descriptor closes, including when
// the process dies, so no lock is ever orphaned and no owner has to be checked
// for liveness.
//
// It is advisory: a process that ignores this protocol can still write to the
// file. It is not distributed coordination, and remote filesystems are outside
// what this supports.
func lockExclusive(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrLocked
	}
	return err
}

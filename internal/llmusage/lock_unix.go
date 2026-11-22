//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package llmusage

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockFile(f *os.File) error {
	for {
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != unix.EINTR {
			return err
		}
	}
}

func unlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}

// LockOutput takes a nonblocking exclusive lock until the output file is closed.
func LockOutput(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) }

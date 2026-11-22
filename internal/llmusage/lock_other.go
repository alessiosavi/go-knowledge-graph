//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package llmusage

import (
	"errors"
	"os"
)

func lockFile(*os.File) error { return errors.New("flock unavailable") }

func unlockFile(*os.File) error { return errors.New("flock unavailable") }

// LockOutput requires an operating system with flock support.
func LockOutput(*os.File) error { return errors.New("flock unavailable") }

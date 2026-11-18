//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package judge

import (
	"os"

	"golang.org/x/sys/unix"
)

func flockRejudgeDir(dir *os.File) error {
	return unix.Flock(int(dir.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

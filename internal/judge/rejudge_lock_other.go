//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package judge

import (
	"fmt"
	"os"
	"runtime"
)

func flockRejudgeDir(_ *os.File) error {
	return fmt.Errorf("rejudge output directory locking is unavailable on %s", runtime.GOOS)
}

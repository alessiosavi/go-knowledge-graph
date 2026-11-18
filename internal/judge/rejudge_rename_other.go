//go:build !linux && !darwin

package judge

import "os"

func renameRejudgeExclusive(from, to string) error {
	// Publish the complete inode without clobbering an existing destination.
	// writeRejudgeAtomic removes the temporary name after publication.
	return os.Link(from, to)
}

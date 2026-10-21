//go:build !linux

package host

import (
	"errors"
	"fmt"
)

// ReadDisk is Linux-only; elsewhere it reports the disk figure as unsupported.
func ReadDisk(path string) (Disk, error) {
	return Disk{}, fmt.Errorf("host: disk size of %s: %w: Sonar reads it with statfs, which needs Linux", path, errors.ErrUnsupported)
}

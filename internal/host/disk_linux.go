package host

import (
	"fmt"
	"syscall"
)

// ReadDisk reads the size of the filesystem holding path.
func ReadDisk(path string) (Disk, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Disk{}, fmt.Errorf("host: statfs %s: %w", path, err)
	}
	size := uint64(st.Bsize)
	return Disk{
		Total:      st.Blocks * size,
		Used:       (st.Blocks - st.Bfree) * size,
		Available:  st.Bavail * size,
		Inodes:     st.Files,
		InodesFree: st.Ffree,
	}, nil
}

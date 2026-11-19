package proxy

import (
	"io/fs"
	"syscall"
)

// fileID tells hard links to one file apart from other files.
type fileID struct{ dev, ino uint64 }

// allocated is the space a file takes on disk (its blocks, not its length:
// a sparse file takes less, a small one a whole block), and, for a file with
// more than one link, its identity.
func allocated(info fs.FileInfo) (int64, fileID, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return info.Size(), fileID{}, false
	}
	return st.Blocks * 512, fileID{dev: uint64(st.Dev), ino: st.Ino}, !info.IsDir() && st.Nlink > 1
}

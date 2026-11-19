package proxy

import (
	"io/fs"
	"syscall"
)

// fileID tells hard links to one file apart from other files.
type fileID struct{ dev, ino uint64 }

// allocated is a file's blocks on disk, not its length, and its identity when it has several links.
func allocated(info fs.FileInfo) (int64, fileID, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return info.Size(), fileID{}, false
	}
	return st.Blocks * 512, fileID{dev: uint64(st.Dev), ino: st.Ino}, !info.IsDir() && st.Nlink > 1
}

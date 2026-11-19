//go:build !linux

package proxy

import "io/fs"

// fileID tells hard links to one file apart; elsewhere than Linux they
// aren't told apart.
type fileID struct{}

// allocated is a file's length: outside Linux, Sonar doesn't read blocks.
func allocated(info fs.FileInfo) (int64, fileID, bool) {
	return info.Size(), fileID{}, false
}

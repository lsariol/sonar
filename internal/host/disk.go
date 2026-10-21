package host

// Disk is a filesystem's size, in bytes, from statfs. In a container, "/"
// reports the filesystem Docker keeps containers on; on Spark that's the
// root disk (confirmed with df).
type Disk struct {
	Total, Used, Available uint64
	Inodes, InodesFree     uint64
}

// UsedPercent is the share in use, as df shows it: space reserved for root
// counts as neither used nor available.
func (d Disk) UsedPercent() float64 {
	if d.Used+d.Available == 0 {
		return 0
	}
	return float64(d.Used) / float64(d.Used+d.Available) * 100
}

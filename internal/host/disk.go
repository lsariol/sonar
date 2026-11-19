package host

// Disk is a filesystem's size from statfs; in a container "/" is the disk Docker keeps containers on.
type Disk struct {
	Total, Used, Available uint64
	Inodes, InodesFree     uint64
}

// UsedPercent is the share in use as df shows it, root's reserve counted as neither.
func (d Disk) UsedPercent() float64 {
	if d.Used+d.Available == 0 {
		return 0
	}
	return float64(d.Used) / float64(d.Used+d.Available) * 100
}

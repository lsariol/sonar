// Package containers is what sonar-proxy answers: the only fields that ever leave it.
package containers

import "time"

// Socket is where sonar-proxy listens, in the proxy volume.
const Socket = "/run/sonar-proxy/proxy.sock"

// The labels the proxy passes on; every other label stays behind.
const (
	ProjectLabel    = "com.docker.compose.project"
	ServiceLabel    = "com.docker.compose.service"
	PublicURLLabel  = "com.mobasity.public-url"
	CheckPathLabel  = "com.mobasity.check-path" // checked instead of the public URL's own path
	VersionLabel    = "com.mobasity.version"
	GroupLabel      = "com.mobasity.group"       // infrastructure (Spark depends on it) or app
	ReportPortLabel = "com.mobasity.report-port" // where the app serves its report (internal/report)
)

// Container is one container: what it is and what state it's in.
type Container struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Project       string     `json:"project"` // "" without compose labels
	Service       string     `json:"service"` // the container name without compose labels
	Image         string     `json:"image"`   // the reference it runs, e.g. postgres:16.4
	State         string     `json:"state"`   // created, running, paused, restarting, removing, exited, dead
	Health        string     `json:"health"`  // healthy, unhealthy, starting, none
	ExitCode      int        `json:"exit_code"`
	RestartCount  int        `json:"restart_count"`
	RestartPolicy string     `json:"restart_policy"` // no, always, unless-stopped, on-failure
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at"`
	CPULimit      float64    `json:"cpu_limit"`    // cores; 0 when unlimited
	MemoryLimit   int64      `json:"memory_limit"` // bytes; 0 when unlimited
	PublicURL     string     `json:"public_url"`
	CheckPath     string     `json:"check_path"`
	Version       string     `json:"version"`
	Group         string     `json:"group"`
	ReportPort    string     `json:"report_port"`
}

// Stats are one running container's counters at ReadAt; rates come from two readings.
type Stats struct {
	ID            string    `json:"id"`
	ReadAt        time.Time `json:"read_at"`
	CPUNanos      uint64    `json:"cpu_ns"`       // CPU time used since the container started
	MemoryBytes   uint64    `json:"memory_bytes"` // in use, page cache excluded
	MemoryLimit   uint64    `json:"memory_limit"`
	NetRxBytes    uint64    `json:"net_rx_bytes"`
	NetTxBytes    uint64    `json:"net_tx_bytes"`
	BlkReadBytes  uint64    `json:"blk_read_bytes"`
	BlkWriteBytes uint64    `json:"blk_write_bytes"`
	PIDs          uint64    `json:"pids"`
}

// ContainersAnswer is GET /v1/containers: every container, and the ones Docker couldn't inspect.
type ContainersAnswer struct {
	Containers []Container      `json:"containers"`
	Failed     []ContainerError `json:"failed"`
}

// ContainerError says why one container is missing from the list.
type ContainerError struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// StatsAnswer is GET /v1/stats: every running container's stats, and the ones that failed.
type StatsAnswer struct {
	Stats  []Stats      `json:"stats"`
	Failed []StatsError `json:"failed"`
}

// StatsError says why one container's stats are missing.
type StatsError struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// DiskAnswer is GET /v1/disk: the latest measurement, MeasuredAt nil until the first ends.
type DiskAnswer struct {
	MeasuredAt  *time.Time      `json:"measured_at"`
	Seconds     float64         `json:"seconds"` // how long it took
	Folders     []FolderSize    `json:"folders"` // each data folder in /srv/server/storage
	FolderError string          `json:"folder_error,omitempty"`
	Projects    []ProjectDocker `json:"projects"` // Docker's figures per compose project
	Totals      DockerTotals    `json:"totals"`
	DockerError string          `json:"docker_error,omitempty"`
}

// FolderSize is one data folder's space and files, and the entries that couldn't be read.
type FolderSize struct {
	Name       string `json:"name"`
	Bytes      int64  `json:"bytes"`
	Files      int64  `json:"files"`
	Unreadable int64  `json:"unreadable"`
}

// ProjectDocker is a project's own image layers, its containers' writable layers and its volumes.
type ProjectDocker struct {
	Project        string `json:"project"`
	ImageBytes     int64  `json:"image_bytes"`
	ContainerBytes int64  `json:"container_bytes"`
	VolumeBytes    int64  `json:"volume_bytes"`
}

// DockerTotals is everything Docker stores, shared layers counted once.
type DockerTotals struct {
	Images     int64 `json:"images"`
	Containers int64 `json:"containers"`
	Volumes    int64 `json:"volumes"`
	BuildCache int64 `json:"build_cache"`
}

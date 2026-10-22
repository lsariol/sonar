// Package containers is what sonar-proxy answers and the agent reads: the
// container list and their stats, as Sonar's own types. Only these fields
// ever leave the proxy.
package containers

import "time"

// Socket is where sonar-proxy listens, in the proxy volume.
const Socket = "/run/sonar-proxy/proxy.sock"

// The labels the proxy passes on; every other label stays behind.
const (
	ProjectLabel   = "com.docker.compose.project"
	ServiceLabel   = "com.docker.compose.service"
	PublicURLLabel = "com.mobasity.public-url"
	VersionLabel   = "com.mobasity.version"
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
	Version       string     `json:"version"`
}

// Stats are one running container's counters at ReadAt. Rates come from the
// difference between two readings.
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

// ContainersAnswer is the answer to GET /v1/containers: every container, and
// the ones Docker couldn't inspect, which are left out of the list.
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

// StatsAnswer is the answer to GET /v1/stats: every running container's
// stats, and the ones that couldn't be read.
type StatsAnswer struct {
	Stats  []Stats      `json:"stats"`
	Failed []StatsError `json:"failed"`
}

// StatsError says why one container's stats are missing.
type StatsError struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

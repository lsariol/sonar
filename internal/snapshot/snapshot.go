// Package snapshot is the agent's latest view, a file the API reads but never serves as is.
package snapshot

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lsariol/sonar/internal/state"
)

const (
	// Dir is where the snapshot lives in the containers (the state volume).
	Dir = "/var/lib/sonar"

	// Format changes only when a reader of the old format would misread it.
	Format = 1

	// FreshFor is how old a snapshot may be before its data counts as unknown.
	FreshFor = 30 * time.Second

	fileName = "snapshot.json"
)

// Collector states.
const (
	Available   = "available"
	Unavailable = "unavailable"
	Stale       = "stale"
	Unsupported = "unsupported"
)

// Snapshot is everything the agent knows right now.
type Snapshot struct {
	Format      int                  `json:"format"`
	GeneratedAt time.Time            `json:"generated_at"`
	Version     string               `json:"version"`
	Collectors  map[string]Collector `json:"collectors"` // host, filesystem, docker, checks
	Host        *Host                `json:"host"`       // nil until the host has been read
	Projects    []Project            `json:"projects"`
	Checks      []Check              `json:"checks"`
	Databases   *DatabaseServer      `json:"databases"`  // nil until sparkdb has been read
	Storage     *DiskUse             `json:"storage"`    // nil until the proxy has measured
	Apps        []AppReport          `json:"apps"`       // the apps' own reports
	Conditions  []Condition          `json:"conditions"` // what's wrong with Spark itself
	Attention   []string             `json:"attention"`  // problems for `sonar status`
}

// DatabaseServer is sparkdb: its connections and each database on it.
type DatabaseServer struct {
	ReadAt         time.Time  `json:"read_at"`
	MaxConnections int        `json:"max_connections"`
	Connections    int        `json:"connections"` // client connections to every database
	Databases      []Database `json:"databases"`
}

// Database is one database on sparkdb.
type Database struct {
	Name           string `json:"name"`
	Project        string `json:"project"` // the compose project it belongs to; "" when none matches
	SizeBytes      int64  `json:"size_bytes"`
	Connections    int    `json:"connections"`
	Active         int    `json:"active"`
	OldestXactSecs *int64 `json:"oldest_xact_secs"` // the oldest open transaction's age
}

// DiskUse is what each project keeps on disk, as the proxy last measured.
type DiskUse struct {
	MeasuredAt time.Time     `json:"measured_at"`
	Seconds    float64       `json:"seconds"`
	Projects   []ProjectDisk `json:"projects"`
	Totals     DiskTotals    `json:"totals"`
}

// ProjectDisk is one project's data folder and what Docker stores for it.
type ProjectDisk struct {
	Project        string `json:"project"`
	DataBytes      *int64 `json:"data_bytes"` // nil without a data folder
	DataFiles      int64  `json:"data_files"`
	DataUnreadable int64  `json:"data_unreadable"` // entries that couldn't be read
	ImageBytes     int64  `json:"image_bytes"`     // layers only its images have
	ContainerBytes int64  `json:"container_bytes"` // its containers' writable layers
	VolumeBytes    int64  `json:"volume_bytes"`
}

// DiskTotals is everything, shared image layers counted once.
type DiskTotals struct {
	Data       *int64 `json:"data"` // nil when the data folders couldn't be read
	Images     int64  `json:"images"`
	Containers int64  `json:"containers"`
	Volumes    int64  `json:"volumes"`
	BuildCache int64  `json:"build_cache"`
}

// AppReport is one container's report as last read.
type AppReport struct {
	ContainerID string            `json:"container_id"`
	Project     string            `json:"project"`
	Service     string            `json:"service"`
	Container   string            `json:"container"`
	ReadAt      *time.Time        `json:"read_at"` // the last good reading
	Error       string            `json:"error,omitempty"`
	StartedAt   time.Time         `json:"started_at"`
	Counters    map[string]uint64 `json:"counters"`
	Checks      []AppCheck        `json:"checks"`
}

// AppCheck is one of an app's features, whether it works, and since when.
type AppCheck struct {
	Name    string    `json:"name"`
	OK      bool      `json:"ok"`
	Message string    `json:"message"`
	Since   time.Time `json:"since"`
}

// Condition levels.
const (
	Warning  = "warning"
	Critical = "critical"
)

// Condition is something wrong with Spark itself, and since when.
type Condition struct {
	ID      string    `json:"id"`      // what kind: disk, memory, project, ...
	Subject string    `json:"subject"` // which one, for kinds with several: a project, a collector; else ""
	Level   string    `json:"level"`   // warning or critical
	Message string    `json:"message"`
	Since   time.Time `json:"since"`
}

// Fresh reports whether the snapshot is recent enough to trust at now.
func (s Snapshot) Fresh(now time.Time) bool {
	return !s.GeneratedAt.IsZero() && now.Sub(s.GeneratedAt) <= FreshFor
}

// Collector is how one source of data is doing.
type Collector struct {
	State  string     `json:"state"`
	LastOK *time.Time `json:"last_ok"`
	Error  string     `json:"error,omitempty"`
}

// Host is the host's latest figures; pointers are nil until known.
type Host struct {
	ReadAt            time.Time `json:"read_at"`
	CPUPercent        *float64  `json:"cpu_pct"`
	CorePercents      []float64 `json:"core_pcts"`
	MemoryTotal       uint64    `json:"memory_total"`
	MemoryUsed        uint64    `json:"memory_used"`
	MemoryUsedPercent float64   `json:"memory_used_pct"`
	SwapTotal         uint64    `json:"swap_total"`
	SwapUsed          uint64    `json:"swap_used"`
	Load1             float64   `json:"load1"`
	Load5             float64   `json:"load5"`
	Load15            float64   `json:"load15"`
	UptimeSeconds     float64   `json:"uptime_seconds"`
	Disk              *Disk     `json:"disk"`
	Network           []NetIf   `json:"network"` // nil without the host's network counters
}

// NetIf is one host interface: its rates (nil until two readings) and its counters.
type NetIf struct {
	Name    string   `json:"name"`
	RxRate  *float64 `json:"rx_rate"`
	TxRate  *float64 `json:"tx_rate"`
	RxBytes uint64   `json:"rx_bytes"`
	TxBytes uint64   `json:"tx_bytes"`
}

// Disk is the filesystem containers are stored on.
type Disk struct {
	Total       uint64  `json:"total"`
	Used        uint64  `json:"used"`
	Available   uint64  `json:"available"`
	UsedPercent float64 `json:"used_pct"`
	Inodes      uint64  `json:"inodes"`
	InodesFree  uint64  `json:"inodes_free"`
}

// Project is a compose project and its services.
type Project struct {
	Project  string         `json:"project"`
	Group    string         `json:"group"` // from the group label: infrastructure, app, or "" when unset
	State    state.State    `json:"state"`
	Evidence state.Evidence `json:"evidence"`
	Services []Service      `json:"services"`
}

// Service is a compose service: its state, its containers, and their usage added up.
type Service struct {
	Service    string         `json:"service"`
	State      state.State    `json:"state"`
	Counted    bool           `json:"counted"` // false for one-off jobs
	Usage      Usage          `json:"usage"`
	Update     *ServiceUpdate `json:"update"` // while a deploy replaces it
	Containers []Container    `json:"containers"`
}

// ServiceUpdate is a deploy in progress: since when, and between which versions.
type ServiceUpdate struct {
	Since time.Time `json:"since"`
	From  string    `json:"from"`
	To    string    `json:"to"`
}

// Container is one container instance.
type Container struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Image        string     `json:"image"`
	Version      string     `json:"version"`
	State        string     `json:"state"`
	Health       string     `json:"health"`
	ExitCode     int        `json:"exit_code"`
	RestartCount int        `json:"restart_count"`
	CreatedAt    time.Time  `json:"created_at"`
	StartedAt    *time.Time `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`
	CPULimit     float64    `json:"cpu_limit"`
	MemoryLimit  int64      `json:"memory_limit"`
	Usage        Usage      `json:"usage"`
}

// Usage is resource use; rates are per second, nil until two readings or after a reset.
type Usage struct {
	CPUCores      *float64 `json:"cpu_cores"`
	MemoryBytes   *uint64  `json:"memory_bytes"`
	NetRxRate     *float64 `json:"net_rx_rate"`
	NetTxRate     *float64 `json:"net_tx_rate"`
	BlkReadRate   *float64 `json:"blk_read_rate"`
	BlkWriteRate  *float64 `json:"blk_write_rate"`
	NetRxBytes    *uint64  `json:"net_rx_bytes"` // counters, for the minute totals
	NetTxBytes    *uint64  `json:"net_tx_bytes"`
	BlkReadBytes  *uint64  `json:"blk_read_bytes"`
	BlkWriteBytes *uint64  `json:"blk_write_bytes"`
}

// Check is one outside check of a public address.
type Check struct {
	Project             string      `json:"project"`
	Service             string      `json:"service"`
	URL                 string      `json:"url"`
	State               state.State `json:"state"`
	LastAt              *time.Time  `json:"last_at"`
	LastOutcome         string      `json:"last_outcome"`
	LastStatus          int         `json:"last_status"`
	LastLatencyMs       *int64      `json:"last_latency_ms"`
	LastError           string      `json:"last_error,omitempty"`
	ConsecutiveFailures int         `json:"consecutive_failures"`
}

// Write replaces the snapshot atomically: a synced temporary file renamed over the old one.
func Write(dir string, s Snapshot) error {
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	tmp, err := os.CreateTemp(dir, fileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("snapshot: %w: is the state volume mounted and writable?", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("snapshot: writing %s: %w", tmp.Name(), err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("snapshot: syncing %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, fileName)); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	return nil
}

// Read reads the snapshot in dir.
func Read(dir string) (Snapshot, error) {
	path := filepath.Join(dir, fileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, fmt.Errorf("snapshot: %s doesn't exist yet: is sonar-agent running?", path)
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("snapshot: %w", err)
	}
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return Snapshot{}, fmt.Errorf("snapshot: %s is unreadable: %w", path, err)
	}
	if s.Format != Format {
		return Snapshot{}, fmt.Errorf("snapshot: %s is format %d, this Sonar reads %d: sonar-agent and sonar-api are different versions", path, s.Format, Format)
	}
	return s, nil
}

// Cache reads the snapshot again only when the file has changed.
type Cache struct {
	dir string

	mu      sync.Mutex
	modTime time.Time
	size    int64
	snap    Snapshot
}

// NewCache returns a cache of the snapshot in dir.
func NewCache(dir string) *Cache {
	return &Cache{dir: dir}
}

// Load returns the latest snapshot.
func (c *Cache) Load() (Snapshot, error) {
	info, err := os.Stat(filepath.Join(c.dir, fileName))
	if err != nil {
		return Read(c.dir)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.modTime.IsZero() && info.ModTime().Equal(c.modTime) && info.Size() == c.size {
		return c.snap, nil
	}
	s, err := Read(c.dir)
	if err != nil {
		return Snapshot{}, err
	}
	c.modTime, c.size, c.snap = info.ModTime(), info.Size(), s
	return s, nil
}

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/lsariol/sonar/internal/containers"
)

// maxDockerAnswer is the largest answer read from Docker for one call.
const maxDockerAnswer = 8 << 20

// errNotFound is Docker's 404: the container went away between two calls.
var errNotFound = errors.New("not found")

// idPattern is a full container ID, checked before one goes into a path.
var idPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// docker is a minimal Docker Engine API client over its socket: the four read
// calls the proxy makes, and nothing else.
type docker struct {
	base string
	http *http.Client
}

func newDocker(socket string) *docker {
	return &docker{
		base: "http://docker",
		http: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
				MaxIdleConnsPerHost: parallel,
			},
		},
	}
}

// listed is one container from the list: enough to pick what to inspect.
type listed struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
}

// name is the container's name without Docker's leading slash.
func (l listed) name() string {
	if len(l.Names) == 0 {
		return ""
	}
	return strings.TrimPrefix(l.Names[0], "/")
}

// inspected holds the inspect fields Sonar uses. Fields not declared here,
// such as Config.Env, Config.Cmd and Mounts, are never read into memory.
type inspected struct {
	ID           string `json:"Id"`
	Name         string `json:"Name"`
	Created      string `json:"Created"`
	RestartCount int    `json:"RestartCount"`
	State        struct {
		Status     string `json:"Status"`
		ExitCode   int    `json:"ExitCode"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
		Health     *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	HostConfig struct {
		RestartPolicy struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
		NanoCpus  int64 `json:"NanoCpus"`
		CpuQuota  int64 `json:"CpuQuota"`
		CpuPeriod int64 `json:"CpuPeriod"`
		Memory    int64 `json:"Memory"`
	} `json:"HostConfig"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

// rawStats holds the one-shot stats fields Sonar uses.
type rawStats struct {
	Read     string `json:"read"`
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
	} `json:"cpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
	BlkioStats struct {
		IOServiceBytesRecursive []struct {
			Op    string `json:"op"`
			Value uint64 `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
	PidsStats struct {
		Current uint64 `json:"current"`
	} `json:"pids_stats"`
}

func (d *docker) ping(ctx context.Context) error {
	return d.get(ctx, "/_ping", nil)
}

// list returns every container except `docker compose run` leftovers.
func (d *docker) list(ctx context.Context) ([]listed, error) {
	var all []listed
	if err := d.get(ctx, "/containers/json?all=1", &all); err != nil {
		return nil, err
	}
	kept := all[:0]
	for _, c := range all {
		if !idPattern.MatchString(c.ID) {
			return nil, fmt.Errorf("Docker: listing containers: unexpected container ID %q", c.ID)
		}
		if c.Labels["com.docker.compose.oneoff"] == "True" {
			continue
		}
		kept = append(kept, c)
	}
	return kept, nil
}

func (d *docker) inspect(ctx context.Context, id string) (inspected, error) {
	var c inspected
	err := d.get(ctx, "/containers/"+id+"/json", &c)
	return c, err
}

func (d *docker) stats(ctx context.Context, id string) (rawStats, error) {
	var s rawStats
	err := d.get(ctx, "/containers/"+id+"/stats?stream=false&one-shot=true", &s)
	return s, err
}

// get fetches path and decodes the JSON answer into v (nil: discard it).
func (d *docker) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("Docker: %w: is the Docker socket mounted at %s?", err, DockerSocket)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errNotFound
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("Docker: GET %s: %s", route(path), resp.Status)
	}
	body := io.LimitReader(resp.Body, maxDockerAnswer)
	if v == nil {
		_, err := io.Copy(io.Discard, body)
		return err
	}
	if err := json.NewDecoder(body).Decode(v); err != nil {
		return fmt.Errorf("Docker: GET %s: unreadable answer: %w", route(path), err)
	}
	return nil
}

// route is path without its query, for error messages.
func route(path string) string {
	route, _, _ := strings.Cut(path, "?")
	return route
}

// toContainer copies the fields Sonar reports, one by one.
func toContainer(c inspected) containers.Container {
	labels := c.Config.Labels
	name := strings.TrimPrefix(c.Name, "/")

	out := containers.Container{
		ID:            c.ID,
		Name:          name,
		Project:       labels[containers.ProjectLabel],
		Service:       labels[containers.ServiceLabel],
		Image:         c.Config.Image,
		State:         c.State.Status,
		Health:        "none",
		ExitCode:      c.State.ExitCode,
		RestartCount:  c.RestartCount,
		RestartPolicy: c.HostConfig.RestartPolicy.Name,
		CreatedAt:     parseTime(c.Created),
		StartedAt:     optionalTime(c.State.StartedAt),
		FinishedAt:    optionalTime(c.State.FinishedAt),
		MemoryLimit:   c.HostConfig.Memory,
		PublicURL:     labels[containers.PublicURLLabel],
		Version:       labels[containers.VersionLabel],
	}
	if out.Project == "" {
		out.Service = name
	}
	if c.State.Health != nil && c.State.Health.Status != "" {
		out.Health = c.State.Health.Status
	}
	if out.RestartPolicy == "" {
		out.RestartPolicy = "no"
	}
	switch {
	case c.HostConfig.NanoCpus > 0:
		out.CPULimit = float64(c.HostConfig.NanoCpus) / 1e9
	case c.HostConfig.CpuQuota > 0 && c.HostConfig.CpuPeriod > 0:
		out.CPULimit = float64(c.HostConfig.CpuQuota) / float64(c.HostConfig.CpuPeriod)
	}
	return out
}

// toStats copies one container's counters. Memory in use leaves out the page
// cache, as `docker stats` does (inactive_file on cgroup v2,
// total_inactive_file on v1).
func toStats(id string, s rawStats) containers.Stats {
	out := containers.Stats{
		ID:          id,
		ReadAt:      parseTime(s.Read),
		CPUNanos:    s.CPUStats.CPUUsage.TotalUsage,
		MemoryBytes: s.MemoryStats.Usage,
		MemoryLimit: s.MemoryStats.Limit,
		PIDs:        s.PidsStats.Current,
	}
	if out.ReadAt.IsZero() {
		out.ReadAt = time.Now().UTC()
	}

	inactive, ok := s.MemoryStats.Stats["inactive_file"]
	if !ok {
		inactive = s.MemoryStats.Stats["total_inactive_file"]
	}
	if inactive < out.MemoryBytes {
		out.MemoryBytes -= inactive
	}

	for _, n := range s.Networks {
		out.NetRxBytes += n.RxBytes
		out.NetTxBytes += n.TxBytes
	}
	for _, b := range s.BlkioStats.IOServiceBytesRecursive {
		switch strings.ToLower(b.Op) {
		case "read":
			out.BlkReadBytes += b.Value
		case "write":
			out.BlkWriteBytes += b.Value
		}
	}
	return out
}

// parseTime reads one of Docker's timestamps; Docker's zero time and
// anything unreadable become the zero time.
func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() <= 1 {
		return time.Time{}
	}
	return t.UTC()
}

func optionalTime(s string) *time.Time {
	t := parseTime(s)
	if t.IsZero() {
		return nil
	}
	return &t
}

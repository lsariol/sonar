// Package host reads the host's CPU, memory, load, uptime and disk. Inside a
// container, /proc/stat, /proc/meminfo, /proc/loadavg and /proc/uptime show
// the whole host (confirmed on Spark), so no host mounts are needed.
package host

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Reader reads the /proc under Root: "/" in a container, a testdata folder in
// tests.
type Reader struct {
	Root string
}

// CPUTimes are cumulative CPU time counters from /proc/stat, in clock ticks.
// Guest time is already counted in User and Nice, so it's left out.
type CPUTimes struct {
	User, Nice, System, Idle, IOWait, IRQ, SoftIRQ, Steal uint64
}

func (t CPUTimes) total() uint64 {
	return t.User + t.Nice + t.System + t.Idle + t.IOWait + t.IRQ + t.SoftIRQ + t.Steal
}

func (t CPUTimes) idle() uint64 {
	return t.Idle + t.IOWait
}

// CPU is one reading of /proc/stat: all cores together, and each core.
type CPU struct {
	Total CPUTimes
	Cores []CPUTimes
}

// Busy is the percentage of CPU time spent working between two readings. It
// reports false when no time passed or a counter went backwards (a reboot).
func Busy(prev, cur CPUTimes) (float64, bool) {
	total, idle := cur.total(), cur.idle()
	if total <= prev.total() || idle < prev.idle() || total-prev.total() < idle-prev.idle() {
		return 0, false
	}
	dTotal, dIdle := total-prev.total(), idle-prev.idle()
	return float64(dTotal-dIdle) / float64(dTotal) * 100, true
}

// Memory is /proc/meminfo's figures, in bytes.
type Memory struct {
	Total, Available, Free, Buffers, Cached, SwapTotal, SwapFree uint64
}

// Used is memory in use: everything that isn't available. Available counts
// page cache the kernel can reclaim, so it's the honest headline figure.
func (m Memory) Used() uint64 {
	if m.Available > m.Total {
		return 0
	}
	return m.Total - m.Available
}

// UsedPercent is Used as a percentage of Total.
func (m Memory) UsedPercent() float64 {
	if m.Total == 0 {
		return 0
	}
	return float64(m.Used()) / float64(m.Total) * 100
}

// SwapUsed is swap in use, in bytes.
func (m Memory) SwapUsed() uint64 {
	if m.SwapFree > m.SwapTotal {
		return 0
	}
	return m.SwapTotal - m.SwapFree
}

// Load is /proc/loadavg: runnable processes averaged over 1, 5 and 15
// minutes. It isn't a percentage: 4.0 on four cores is fully busy.
type Load struct {
	One, Five, Fifteen float64
}

// CPU reads /proc/stat.
func (r Reader) CPU() (CPU, error) {
	data, err := r.read("stat")
	if err != nil {
		return CPU{}, err
	}

	var cpu CPU
	found := false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "cpu") {
			continue
		}
		times, err := parseCPUTimes(fields[1:])
		if err != nil {
			return CPU{}, fmt.Errorf("host: %s: %s: %w", r.path("stat"), fields[0], err)
		}
		if fields[0] == "cpu" {
			cpu.Total, found = times, true
		} else {
			cpu.Cores = append(cpu.Cores, times)
		}
	}
	if !found {
		return CPU{}, fmt.Errorf("host: %s has no \"cpu\" line", r.path("stat"))
	}
	return cpu, nil
}

// parseCPUTimes reads a cpu line's counters. Kernels before 2.6.11 have fewer
// than eight; the missing ones stay zero.
func parseCPUTimes(fields []string) (CPUTimes, error) {
	if len(fields) < 4 {
		return CPUTimes{}, fmt.Errorf("%d counters, want at least 4", len(fields))
	}
	var v [8]uint64
	for i := 0; i < len(v) && i < len(fields); i++ {
		n, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			return CPUTimes{}, fmt.Errorf("counter %d: %w", i+1, err)
		}
		v[i] = n
	}
	return CPUTimes{User: v[0], Nice: v[1], System: v[2], Idle: v[3], IOWait: v[4], IRQ: v[5], SoftIRQ: v[6], Steal: v[7]}, nil
}

// Memory reads /proc/meminfo.
func (r Reader) Memory() (Memory, error) {
	data, err := r.read("meminfo")
	if err != nil {
		return Memory{}, err
	}

	kb := make(map[string]uint64)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		key, rest, ok := strings.Cut(scanner.Text(), ":")
		fields := strings.Fields(rest)
		if !ok || len(fields) != 2 || fields[1] != "kB" {
			continue
		}
		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return Memory{}, fmt.Errorf("host: %s: %s: %w", r.path("meminfo"), key, err)
		}
		kb[key] = n
	}
	for _, key := range []string{"MemTotal", "MemAvailable"} {
		if _, ok := kb[key]; !ok {
			return Memory{}, fmt.Errorf("host: %s has no %s: Sonar needs Linux 3.14 or newer", r.path("meminfo"), key)
		}
	}
	return Memory{
		Total:     kb["MemTotal"] * 1024,
		Available: kb["MemAvailable"] * 1024,
		Free:      kb["MemFree"] * 1024,
		Buffers:   kb["Buffers"] * 1024,
		Cached:    kb["Cached"] * 1024,
		SwapTotal: kb["SwapTotal"] * 1024,
		SwapFree:  kb["SwapFree"] * 1024,
	}, nil
}

// Load reads /proc/loadavg.
func (r Reader) Load() (Load, error) {
	data, err := r.read("loadavg")
	if err != nil {
		return Load{}, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return Load{}, fmt.Errorf("host: %s: %d fields, want at least 3", r.path("loadavg"), len(fields))
	}
	var v [3]float64
	for i := range v {
		if v[i], err = strconv.ParseFloat(fields[i], 64); err != nil {
			return Load{}, fmt.Errorf("host: %s: %w", r.path("loadavg"), err)
		}
	}
	return Load{One: v[0], Five: v[1], Fifteen: v[2]}, nil
}

// Uptime reads /proc/uptime: how long since the host booted.
func (r Reader) Uptime() (time.Duration, error) {
	data, err := r.read("uptime")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, fmt.Errorf("host: %s is empty", r.path("uptime"))
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || seconds < 0 {
		return 0, fmt.Errorf("host: %s: unreadable uptime %q", r.path("uptime"), fields[0])
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func (r Reader) path(name string) string {
	return filepath.Join(r.Root, "proc", name)
}

func (r Reader) read(name string) ([]byte, error) {
	data, err := os.ReadFile(r.path(name))
	if err != nil {
		return nil, fmt.Errorf("host: %w: Sonar reads the host's figures from /proc, which needs Linux", err)
	}
	return data, nil
}

package host

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// NetDevPath is where the agent reads the host's network counters: the
// host's /proc/1/net/dev, mounted read-only on its own (the rest of
// /proc/1/net lists the host's connections, routes and neighbours). A container's own /proc/net/dev
// shows only the container's network.
const NetDevPath = "/host/net/dev"

// Interface is one network interface's byte counters since boot.
type Interface struct {
	Name             string
	RxBytes, TxBytes uint64
}

// ReadNetwork reads a /proc/net/dev file: every interface but the loopback
// and the veth ends of containers (their traffic also crosses a bridge).
// Without the file, the error wraps errors.ErrUnsupported.
func ReadNetwork(path string) ([]Interface, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("host: network: %w: %s isn't there; mount the host's /proc/1/net/dev there read-only (/proc/1/net/dev:%[2]s:ro)",
			errors.ErrUnsupported, path)
	}
	if err != nil {
		return nil, fmt.Errorf("host: network: %w", err)
	}
	return parseNetDev(path, data)
}

func parseNetDev(path string, data []byte) ([]Interface, error) {
	var out []Interface
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		name, rest, ok := strings.Cut(scanner.Text(), ":")
		name = strings.TrimSpace(name)
		if !ok || strings.Contains(name, "|") {
			continue // the two header lines
		}
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			return nil, fmt.Errorf("host: %s: %s: %d counters, want 16", path, name, len(fields))
		}
		rx, err1 := strconv.ParseUint(fields[0], 10, 64)
		tx, err2 := strconv.ParseUint(fields[8], 10, 64)
		if err := errors.Join(err1, err2); err != nil {
			return nil, fmt.Errorf("host: %s: %s: %w", path, name, err)
		}
		if name == "lo" || strings.HasPrefix(name, "veth") {
			continue
		}
		out = append(out, Interface{Name: name, RxBytes: rx, TxBytes: tx})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("host: %s lists no network interface", path)
	}
	return out, nil
}

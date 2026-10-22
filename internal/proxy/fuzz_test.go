package proxy

import (
	"encoding/json"
	"testing"
)

// FuzzStats: no stats answer from Docker crashes the conversion, and memory in
// use never exceeds Docker's usage figure.
func FuzzStats(f *testing.F) {
	f.Add(`{"memory_stats":{"usage":100,"stats":{"inactive_file":30}},"networks":{"eth0":{"rx_bytes":1}}}`)
	f.Add(`{"memory_stats":{"usage":10,"stats":{"inactive_file":300}}}`)
	f.Fuzz(func(t *testing.T, raw string) {
		var s rawStats
		if json.Unmarshal([]byte(raw), &s) != nil {
			return
		}
		out := toStats("x", s)
		if out.MemoryBytes > s.MemoryStats.Usage {
			t.Fatalf("memory %d over usage %d", out.MemoryBytes, s.MemoryStats.Usage)
		}
	})
}

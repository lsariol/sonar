package host

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// spark reads the /proc files copied from Spark on 2026-10-21.
var spark = Reader{Root: filepath.Join("testdata", "spark")}

func TestSparkCPU(t *testing.T) {
	cpu, err := spark.CPU()
	if err != nil {
		t.Fatal(err)
	}
	want := CPUTimes{User: 23830456, Nice: 25579, System: 16322006, Idle: 2659329804, IOWait: 3480493, SoftIRQ: 577724}
	if cpu.Total != want {
		t.Errorf("Total = %+v\nwant %+v", cpu.Total, want)
	}
	if len(cpu.Cores) != 4 {
		t.Fatalf("%d cores, want Spark's 4", len(cpu.Cores))
	}
	if cpu.Cores[3].User != 5976527 {
		t.Errorf("core 3 user = %d", cpu.Cores[3].User)
	}
}

func TestSparkMemory(t *testing.T) {
	m, err := spark.Memory()
	if err != nil {
		t.Fatal(err)
	}
	if m.Total != 16231800*1024 || m.Available != 12905644*1024 || m.Cached != 10795876*1024 {
		t.Errorf("Memory = %+v", m)
	}
	if got, want := m.Used(), uint64(16231800-12905644)*1024; got != want {
		t.Errorf("Used = %d, want %d", got, want)
	}
	if got := m.UsedPercent(); math.Abs(got-20.49) > 0.01 {
		t.Errorf("UsedPercent = %.3f, want about 20.49", got)
	}
	if got, want := m.SwapUsed(), uint64(1000444-999932)*1024; got != want {
		t.Errorf("SwapUsed = %d, want %d", got, want)
	}
}

func TestSparkLoadAndUptime(t *testing.T) {
	load, err := spark.Load()
	if err != nil {
		t.Fatal(err)
	}
	if load != (Load{One: 0.43, Five: 0.19, Fifteen: 0.12}) {
		t.Errorf("Load = %+v", load)
	}
	up, err := spark.Uptime()
	if err != nil {
		t.Fatal(err)
	}
	if want := 6856871270 * time.Millisecond; up != want {
		t.Errorf("Uptime = %v, want %v", up, want)
	}
}

// TestDockerDesktop reads a different kernel's files (Docker Desktop's WSL VM).
func TestDockerDesktop(t *testing.T) {
	r := Reader{Root: filepath.Join("testdata", "docker-desktop")}
	cpu, err := r.CPU()
	if err != nil || len(cpu.Cores) == 0 || cpu.Total.Idle == 0 {
		t.Errorf("CPU = %+v, %v", cpu, err)
	}
	m, err := r.Memory()
	if err != nil || m.Total == 0 || m.Available == 0 || m.Available > m.Total {
		t.Errorf("Memory = %+v, %v", m, err)
	}
	if _, err := r.Load(); err != nil {
		t.Error(err)
	}
	if _, err := r.Uptime(); err != nil {
		t.Error(err)
	}
}

func TestBusy(t *testing.T) {
	// Spark's host cpu line, then the same counters read from inside a
	// container moments later.
	host := CPUTimes{User: 23830456, Nice: 25579, System: 16322006, Idle: 2659329804, IOWait: 3480493, SoftIRQ: 577724}
	later := CPUTimes{User: 23830475, Nice: 25580, System: 16322027, Idle: 2659329865, IOWait: 3480505, SoftIRQ: 577724}
	got, ok := Busy(host, later)
	if want := 41.0 / 114.0 * 100; !ok || math.Abs(got-want) > 1e-9 {
		t.Errorf("Busy = %.4f, %v; want %.4f", got, ok, want)
	}

	if got, ok := Busy(CPUTimes{User: 100, Idle: 900}, CPUTimes{User: 100, Idle: 1000}); !ok || got != 0 {
		t.Errorf("idle interval: %v, %v; want 0", got, ok)
	}
	if got, ok := Busy(CPUTimes{User: 100, Idle: 900}, CPUTimes{User: 200, Idle: 900}); !ok || got != 100 {
		t.Errorf("busy interval: %v, %v; want 100", got, ok)
	}
	if _, ok := Busy(later, later); ok {
		t.Error("no time passed: want ok false")
	}
	if _, ok := Busy(later, host); ok {
		t.Error("counters went backwards (a reboot): want ok false")
	}
}

func TestErrors(t *testing.T) {
	missing := Reader{Root: t.TempDir()}
	if _, err := missing.CPU(); err == nil || !strings.Contains(err.Error(), "host:") {
		t.Errorf("missing /proc/stat: err = %v", err)
	}

	cases := map[string]struct{ file, content, want string }{
		"no cpu line":     {"stat", "intr 1 2 3\n", `no "cpu" line`},
		"short cpu line":  {"stat", "cpu 1 2\n", "want at least 4"},
		"bad counter":     {"stat", "cpu 1 2 x 4\n", "counter 3"},
		"no MemAvailable": {"meminfo", "MemTotal: 100 kB\nMemFree: 50 kB\n", "Linux 3.14"},
		"short loadavg":   {"loadavg", "0.1 0.2\n", "want at least 3"},
		"bad uptime":      {"uptime", "soon 1\n", "unreadable uptime"},
	}
	for name, tc := range cases {
		root := t.TempDir()
		os.MkdirAll(filepath.Join(root, "proc"), 0o755)
		os.WriteFile(filepath.Join(root, "proc", tc.file), []byte(tc.content), 0o644)
		r := Reader{Root: root}

		var err error
		switch tc.file {
		case "stat":
			_, err = r.CPU()
		case "meminfo":
			_, err = r.Memory()
		case "loadavg":
			_, err = r.Load()
		case "uptime":
			_, err = r.Uptime()
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
}

func TestDiskUsedPercent(t *testing.T) {
	// Spark's root disk, from df -B1 on 2026-10-21: df rounded it to 13%.
	d := Disk{Total: 249365385216, Used: 28817797120, Available: 207805923328}
	if got := d.UsedPercent(); math.Abs(got-12.18) > 0.01 {
		t.Errorf("UsedPercent = %.3f, want about 12.18", got)
	}
	if (Disk{}).UsedPercent() != 0 {
		t.Error("empty disk: want 0")
	}
}

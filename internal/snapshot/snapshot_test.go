package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lsariol/sonar/internal/state"
)

func sample(at time.Time) Snapshot {
	cpu := 12.5
	return Snapshot{
		Format:      Format,
		GeneratedAt: at,
		Version:     "v0.1.0",
		Collectors:  map[string]Collector{"host": {State: Available, LastOK: &at}},
		Host:        &Host{ReadAt: at, CPUPercent: &cpu, MemoryTotal: 100, MemoryUsed: 20, MemoryUsedPercent: 20},
		Projects: []Project{{Project: "plop", State: state.Up, Evidence: state.Checked,
			Services: []Service{{Service: "plop", State: state.Up, Counted: true}}}},
	}
}

func TestWriteRead(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 21, 15, 4, 5, 0, time.UTC)
	if err := Write(dir, sample(at)); err != nil {
		t.Fatal(err)
	}
	if err := Write(dir, sample(at.Add(5*time.Second))); err != nil {
		t.Fatalf("replacing: %v", err)
	}

	s, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.GeneratedAt.Equal(at.Add(5*time.Second)) || *s.Host.CPUPercent != 12.5 || s.Projects[0].State != state.Up {
		t.Errorf("read back %+v", s)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != fileName {
		t.Errorf("folder holds %v, want only %s (no temporary files left)", entries, fileName)
	}
}

func TestReadErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := Read(dir); err == nil || !strings.Contains(err.Error(), "is sonar-agent running") {
		t.Errorf("missing: err = %v", err)
	}

	os.WriteFile(filepath.Join(dir, fileName), []byte(`{"format": 2}`), 0o644)
	if _, err := Read(dir); err == nil || !strings.Contains(err.Error(), "different versions") {
		t.Errorf("other format: err = %v", err)
	}

	os.WriteFile(filepath.Join(dir, fileName), []byte(`{"form`), 0o644)
	if _, err := Read(dir); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("half a file: err = %v", err)
	}
}

func TestFresh(t *testing.T) {
	at := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	s := sample(at)
	if !s.Fresh(at.Add(FreshFor)) || s.Fresh(at.Add(FreshFor+time.Second)) || (Snapshot{}).Fresh(at) {
		t.Error("Fresh is wrong at the 30 s boundary or for an empty snapshot")
	}
}

func TestCache(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	Write(dir, sample(at))

	c := NewCache(dir)
	first, err := c.Load()
	if err != nil || !first.GeneratedAt.Equal(at) {
		t.Fatalf("first load: %v, %v", first.GeneratedAt, err)
	}

	later := at.Add(time.Minute)
	Write(dir, sample(later))
	// Make sure the new file's time differs even on coarse file systems.
	os.Chtimes(filepath.Join(dir, fileName), later, later)
	second, err := c.Load()
	if err != nil || !second.GeneratedAt.Equal(later) {
		t.Errorf("after a change: %v, %v; want the new snapshot", second.GeneratedAt, err)
	}
}

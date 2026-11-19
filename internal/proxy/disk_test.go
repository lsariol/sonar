package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lsariol/sonar/internal/containers"
)

func TestFolderSize(t *testing.T) {
	root := t.TempDir()
	write := func(path string, n int) {
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "lys", "a"), 10000)
	write(filepath.Join(root, "lys", "deep", "b"), 5000)
	write(filepath.Join(root, "plop", "c"), 1)
	os.MkdirAll(filepath.Join(root, "empty"), 0o755)
	write(filepath.Join(root, "not-a-folder"), 99)

	folders, err := measureFolders(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]containers.FolderSize{}
	for _, f := range folders {
		got[f.Name] = f
	}
	if len(folders) != 3 || got["lys"].Files != 2 || got["plop"].Files != 1 || got["empty"].Files != 0 {
		t.Fatalf("folders = %+v", folders)
	}
	// Linux counts blocks, so at least the bytes written; elsewhere exactly.
	if got["lys"].Bytes < 15000 || got["plop"].Bytes < 1 || got["lys"].Unreadable != 0 {
		t.Errorf("sizes = %+v", got)
	}

	if _, err := measureFolders(context.Background(), filepath.Join(root, "missing")); err == nil {
		t.Error("a missing root: no error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := folderSize(ctx, filepath.Join(root, "lys")); err == nil {
		t.Error("a cancelled walk: no error")
	}
}

func TestDockerUse(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "df.json"))
	if err != nil {
		t.Fatal(err)
	}
	var df systemDF
	if err := json.Unmarshal(data, &df); err != nil {
		t.Fatal(err)
	}
	projects, totals := dockerUse(df)
	want := []containers.ProjectDocker{
		{Project: "plop", ImageBytes: 100000000, ContainerBytes: 12288}, // its image once, without the shared layers
		{Project: "sonar", VolumeBytes: 65536},
		{Project: "sparkdb", ImageBytes: 450000000, ContainerBytes: 100},
	}
	if len(projects) != len(want) {
		t.Fatalf("projects = %+v", projects)
	}
	for i := range want {
		if projects[i] != want[i] {
			t.Errorf("project %d = %+v, want %+v", i, projects[i], want[i])
		}
	}
	if totals != (containers.DockerTotals{Images: 2500000000, Containers: 12395, Volumes: 66536, BuildCache: 700000000}) {
		t.Errorf("totals = %+v", totals)
	}
}

// TestDiskEndpoint: the last measurement answers at once, and no other Docker field leaves the proxy.
func TestDiskEndpoint(t *testing.T) {
	fake, s, srv := setupServer(t)
	status, body := get(t, srv, "GET", "/v1/disk")
	if status != http.StatusOK || !strings.Contains(body, `"measured_at":null`) {
		t.Errorf("before measuring: %d %s", status, body)
	}

	// Measure as the background loop does, against the fake daemon.
	measured := s.disk.measure(context.Background())
	s.disk.mu.Lock()
	s.disk.last = measured
	s.disk.mu.Unlock()

	status, body = get(t, srv, "GET", "/v1/disk")
	var answer containers.DiskAnswer
	if err := json.Unmarshal([]byte(body), &answer); err != nil || status != http.StatusOK {
		t.Fatalf("%d %s", status, body)
	}
	if answer.MeasuredAt == nil || answer.DockerError != "" || len(answer.Projects) != 3 || answer.Totals.BuildCache != 700000000 {
		t.Errorf("answer = %s", body)
	}
	for _, leak := range []string{"SECRET-MARKER", "plop-plop", "sha256", "sonar_state"} {
		if strings.Contains(body, leak) {
			t.Errorf("/v1/disk leaks %q", leak)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !strings.Contains(strings.Join(fake.calls, " "), "GET /system/df") {
		t.Errorf("Docker calls = %v, want /system/df", fake.calls)
	}
}

// TestDiskMeterRuns: the loop measures once the first wait is over.
func TestDiskMeterRuns(t *testing.T) {
	_, s, _ := setupServer(t)
	meter := s.disk
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go meter.run(ctx, time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for meter.latest().MeasuredAt == nil {
		if time.Now().After(deadline) {
			t.Fatal("no measurement after 5 s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

package proxy

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/lsariol/sonar/internal/containers"
)

const (
	// StorageDir is where the compose file mounts /srv/server/storage, read-only.
	StorageDir = "/host/storage"

	// diskEvery is how often the proxy measures; walking folders and asking Docker take a while.
	diskEvery = 30 * time.Minute

	// diskFirstAfter lets the containers settle before the first measurement.
	diskFirstAfter = time.Minute

	diskTimeout = 10 * time.Minute
)

// diskMeter measures disk use in the background and keeps the latest result.
type diskMeter struct {
	root   string
	docker *docker

	mu   sync.Mutex
	last containers.DiskAnswer
}

func newDiskMeter(root string, d *docker) *diskMeter {
	return &diskMeter{root: root, docker: d,
		last: containers.DiskAnswer{Folders: []containers.FolderSize{}, Projects: []containers.ProjectDocker{}}}
}

// latest is the last measurement.
func (m *diskMeter) latest() containers.DiskAnswer {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last
}

// run measures after diskFirstAfter, then every diskEvery, until ctx ends.
func (m *diskMeter) run(ctx context.Context, first time.Duration) {
	wait := first
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = diskEvery
		mctx, cancel := context.WithTimeout(ctx, diskTimeout)
		answer := m.measure(mctx)
		cancel()
		m.mu.Lock()
		m.last = answer
		m.mu.Unlock()
	}
}

// measure walks every data folder and asks Docker for its sizes, either failing on its own.
func (m *diskMeter) measure(ctx context.Context) containers.DiskAnswer {
	start := time.Now()
	answer := containers.DiskAnswer{Folders: []containers.FolderSize{}, Projects: []containers.ProjectDocker{}}

	folders, err := measureFolders(ctx, m.root)
	if err != nil {
		answer.FolderError = err.Error()
	} else {
		answer.Folders = folders
	}

	var df systemDF
	if err := m.docker.getSlow(ctx, "/system/df", &df); err != nil {
		answer.DockerError = err.Error()
	} else {
		answer.Projects, answer.Totals = dockerUse(df)
	}

	at := time.Now().UTC()
	answer.MeasuredAt, answer.Seconds = &at, time.Since(start).Seconds()
	return answer
}

// measureFolders sizes each folder directly under root.
func measureFolders(ctx context.Context, root string) ([]containers.FolderSize, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	out := []containers.FolderSize{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		size, err := folderSize(ctx, filepath.Join(root, e.Name()))
		if err != nil {
			return nil, err
		}
		size.Name = e.Name()
		out = append(out, size)
	}
	return out, nil
}

// folderSize adds up the space under dir, links not followed, hard links once, unreadable entries counted.
func folderSize(ctx context.Context, dir string) (containers.FolderSize, error) {
	var size containers.FolderSize
	seen := make(map[fileID]bool)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			size.Unreadable++
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			size.Unreadable++
			return nil
		}
		bytes, id, linked := allocated(info)
		if linked {
			if seen[id] {
				return nil
			}
			seen[id] = true
		}
		size.Bytes += bytes
		if info.Mode().IsRegular() {
			size.Files++
		}
		return nil
	})
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return size, err
	}
	return size, nil
}

// systemDF holds the fields of Docker's GET /system/df that Sonar uses.
type systemDF struct {
	LayersSize int64 `json:"LayersSize"`
	Images     []struct {
		ID         string `json:"Id"`
		Size       int64  `json:"Size"`
		SharedSize int64  `json:"SharedSize"`
	} `json:"Images"`
	Containers []struct {
		ImageID string            `json:"ImageID"`
		SizeRw  int64             `json:"SizeRw"`
		Labels  map[string]string `json:"Labels"`
	} `json:"Containers"`
	Volumes []struct {
		Labels    map[string]string `json:"Labels"`
		UsageData *struct {
			Size int64 `json:"Size"`
		} `json:"UsageData"`
	} `json:"Volumes"`
	BuildCache []struct {
		Size   int64 `json:"Size"`
		Shared bool  `json:"Shared"`
	} `json:"BuildCache"`
}

// dockerUse adds Docker's sizes up per project: unshared image layers, writable layers, volumes.
func dockerUse(df systemDF) ([]containers.ProjectDocker, containers.DockerTotals) {
	unique := make(map[string]int64, len(df.Images))
	for _, img := range df.Images {
		unique[img.ID] = img.Size - max(img.SharedSize, 0)
	}
	byProject := make(map[string]*containers.ProjectDocker)
	of := func(project string) *containers.ProjectDocker {
		p := byProject[project]
		if p == nil {
			p = &containers.ProjectDocker{Project: project}
			byProject[project] = p
		}
		return p
	}

	var totals containers.DockerTotals
	totals.Images = df.LayersSize
	imagesOf := make(map[string]map[string]bool)
	for _, c := range df.Containers {
		totals.Containers += max(c.SizeRw, 0)
		project := c.Labels[containers.ProjectLabel]
		if project == "" {
			continue
		}
		of(project).ContainerBytes += max(c.SizeRw, 0)
		if imagesOf[project] == nil {
			imagesOf[project] = make(map[string]bool)
		}
		imagesOf[project][c.ImageID] = true
	}
	for project, images := range imagesOf {
		for id := range images {
			of(project).ImageBytes += max(unique[id], 0)
		}
	}
	for _, v := range df.Volumes {
		if v.UsageData == nil || v.UsageData.Size < 0 {
			continue
		}
		totals.Volumes += v.UsageData.Size
		if project := v.Labels[containers.ProjectLabel]; project != "" {
			of(project).VolumeBytes += v.UsageData.Size
		}
	}
	for _, b := range df.BuildCache {
		if !b.Shared {
			totals.BuildCache += max(b.Size, 0)
		}
	}

	out := make([]containers.ProjectDocker, 0, len(byProject))
	for _, p := range byProject {
		out = append(out, *p)
	}
	slices.SortFunc(out, func(a, b containers.ProjectDocker) int { return cmp.Compare(a.Project, b.Project) })
	return out, totals
}

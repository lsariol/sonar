package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/lsariol/sonar/internal/containers"
	"github.com/lsariol/sonar/internal/snapshot"
	"github.com/lsariol/sonar/internal/store"
)

const (
	storageCollector = "storage"

	// storageEvery is how often the agent asks the proxy; the proxy measures
	// every 30 minutes, so most answers are the one already seen.
	storageEvery = 5 * time.Minute
)

// DiskReader is where the agent gets disk use per project: sonar-proxy.
type DiskReader interface {
	Disk(ctx context.Context) (containers.DiskAnswer, error)
}

// sampleStorage asks the proxy for its latest measurement and files it once.
func (a *Agent) sampleStorage(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, dockerTimeout)
	defer cancel()
	answer, err := a.opts.Disk.Disk(ctx)

	a.mu.Lock()
	defer a.mu.Unlock()
	if err == nil && answer.MeasuredAt == nil {
		a.record(storageCollector, nil) // the proxy hasn't finished its first measurement
		return
	}
	if err == nil {
		use := diskUse(answer)
		a.storage = &use
		err = storageProblem(answer)
		if a.storageFiled.Before(use.MeasuredAt) {
			a.storageFiled = use.MeasuredAt
			a.bucketAt(a.opts.Now()).disk = &use
		}
	}
	a.record(storageCollector, err)
}

// storageProblem is what went wrong in a measurement, nil when nothing did.
func storageProblem(d containers.DiskAnswer) error {
	var errs []error
	if d.FolderError != "" {
		errs = append(errs, fmt.Errorf("data folders: %s: is /srv/server/storage mounted in sonar-proxy?", d.FolderError))
	}
	if d.DockerError != "" {
		errs = append(errs, fmt.Errorf("Docker's sizes: %s", d.DockerError))
	}
	for _, f := range d.Folders {
		if f.Unreadable > 0 {
			errs = append(errs, fmt.Errorf("%d entries in the data folder %q couldn't be read, so its size is short: does sonar-proxy have CAP_DAC_READ_SEARCH?", f.Unreadable, f.Name))
		}
	}
	return errors.Join(errs...)
}

// diskUse joins the data folders (named after their compose project) and
// Docker's figures into one row per project.
func diskUse(d containers.DiskAnswer) snapshot.DiskUse {
	use := snapshot.DiskUse{MeasuredAt: d.MeasuredAt.UTC(), Seconds: d.Seconds, Projects: []snapshot.ProjectDisk{},
		Totals: snapshot.DiskTotals{Images: d.Totals.Images, Containers: d.Totals.Containers, Volumes: d.Totals.Volumes,
			BuildCache: d.Totals.BuildCache}}
	byProject := make(map[string]*snapshot.ProjectDisk)
	of := func(name string) *snapshot.ProjectDisk {
		p := byProject[name]
		if p == nil {
			p = &snapshot.ProjectDisk{Project: name}
			byProject[name] = p
		}
		return p
	}
	if d.FolderError == "" {
		data := int64(0)
		for _, f := range d.Folders {
			p := of(f.Name)
			bytes := f.Bytes
			p.DataBytes, p.DataFiles, p.DataUnreadable = &bytes, f.Files, f.Unreadable
			data += f.Bytes
		}
		use.Totals.Data = &data
	}
	for _, dp := range d.Projects {
		p := of(dp.Project)
		p.ImageBytes, p.ContainerBytes, p.VolumeBytes = dp.ImageBytes, dp.ContainerBytes, dp.VolumeBytes
	}
	for _, p := range byProject {
		use.Projects = append(use.Projects, *p)
	}
	slices.SortFunc(use.Projects, func(x, y snapshot.ProjectDisk) int { return cmp.Compare(x.Project, y.Project) })
	return use
}

// diskMeasurement is a measurement as the store keeps it.
func diskMeasurement(u *snapshot.DiskUse) *store.DiskMeasurement {
	if u == nil {
		return nil
	}
	m := &store.DiskMeasurement{MeasuredAt: u.MeasuredAt, Data: u.Totals.Data, Images: u.Totals.Images,
		Containers: u.Totals.Containers, Volumes: u.Totals.Volumes, BuildCache: u.Totals.BuildCache}
	for _, p := range u.Projects {
		m.Projects = append(m.Projects, store.ProjectDiskRow{Project: p.Project, DataBytes: p.DataBytes,
			DataUnreadable: p.DataUnreadable, ImageBytes: p.ImageBytes, ContainerBytes: p.ContainerBytes, VolumeBytes: p.VolumeBytes})
	}
	return m
}

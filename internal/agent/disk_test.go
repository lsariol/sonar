package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lsariol/sonar/internal/containers"
	"github.com/lsariol/sonar/internal/snapshot"
)

type fakeDisk struct {
	answer containers.DiskAnswer
	err    error
}

func (f *fakeDisk) Disk(context.Context) (containers.DiskAnswer, error) { return f.answer, f.err }

// TestStorage: folders and Docker's figures join per project, a measurement is filed once, unreadable ones reported.
func TestStorage(t *testing.T) {
	d := &fakeDisk{}
	a, clk := newTestAgent(t, &fakeDocker{}, &fakeChecker{})
	a.opts.Disk = d
	a.collectors[storageCollector] = &collector{every: storageEvery}

	a.sampleStorage(context.Background())
	if s := a.build(); s.Storage != nil || s.Collectors[storageCollector].State != snapshot.Available {
		t.Fatalf("before the proxy's first measurement: %+v %+v", s.Storage, s.Collectors[storageCollector])
	}

	measured := clk.t.Add(-time.Minute)
	d.answer = containers.DiskAnswer{MeasuredAt: &measured, Seconds: 3,
		Folders:  []containers.FolderSize{{Name: "lys", Bytes: 5000, Files: 3}, {Name: "sparkdb", Bytes: 9000, Files: 9, Unreadable: 2}},
		Projects: []containers.ProjectDocker{{Project: "lys", ImageBytes: 100}, {Project: "plop", ContainerBytes: 7}},
		Totals:   containers.DockerTotals{Images: 1000, BuildCache: 50}}
	a.sampleStorage(context.Background())
	s := a.build()
	if s.Storage == nil || len(s.Storage.Projects) != 3 || *s.Storage.Totals.Data != 14000 || s.Storage.Totals.BuildCache != 50 {
		t.Fatalf("storage = %+v", s.Storage)
	}
	lys, plop := s.Storage.Projects[0], s.Storage.Projects[1]
	if lys.Project != "lys" || *lys.DataBytes != 5000 || lys.ImageBytes != 100 || plop.DataBytes != nil || plop.ContainerBytes != 7 {
		t.Errorf("lys = %+v, plop = %+v", lys, plop)
	}
	col := s.Collectors[storageCollector]
	if col.State != snapshot.Unavailable || !strings.Contains(col.Error, `2 entries in the data folder "sparkdb"`) {
		t.Errorf("collector = %+v, want sparkdb's unreadable entries reported", col)
	}

	m := a.bucketAt(clk.t).minute()
	if m.Disk == nil || len(m.Disk.Projects) != 3 || !m.Disk.MeasuredAt.Equal(measured) {
		t.Fatalf("minute disk = %+v", m.Disk)
	}
	clk.t = clk.t.Add(time.Minute)
	a.sampleStorage(context.Background()) // the same measurement again
	if m := a.bucketAt(clk.t).minute(); m.Disk != nil {
		t.Errorf("the same measurement was filed twice")
	}

	d.answer.FolderError = "open /host/storage: no such file or directory"
	later := measured.Add(30 * time.Minute)
	d.answer.MeasuredAt = &later
	a.sampleStorage(context.Background())
	if s := a.build(); s.Storage.Totals.Data != nil || !strings.Contains(s.Collectors[storageCollector].Error, "is /srv/server/storage mounted") {
		t.Errorf("without the folders: %+v, %+v", s.Storage.Totals, s.Collectors[storageCollector])
	}
}

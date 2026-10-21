package host

import "testing"

func TestReadDisk(t *testing.T) {
	d, err := ReadDisk("/")
	if err != nil {
		t.Fatal(err)
	}
	if d.Total == 0 || d.Used > d.Total || d.Available > d.Total || d.Inodes == 0 {
		t.Errorf("ReadDisk(/) = %+v", d)
	}
	if _, err := ReadDisk("/no/such/path"); err == nil {
		t.Error("missing path: want an error")
	}
}

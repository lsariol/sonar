package host

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestReadNetwork(t *testing.T) {
	got, err := ReadNetwork(filepath.Join("testdata", "spark", "net", "dev"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Interface{
		{Name: "enp3s0", RxBytes: 98234881234, TxBytes: 12345678901},
		{Name: "docker0", RxBytes: 1203568, TxBytes: 140990483},
		{Name: "br-1a2b3c4d5e6f", RxBytes: 4567890123, TxBytes: 5678901234},
	}
	if !slices.Equal(got, want) {
		t.Errorf("interfaces = %+v, want %+v (no lo, no veth)", got, want)
	}

	_, err = ReadNetwork(filepath.Join(t.TempDir(), "dev"))
	if !errors.Is(err, errors.ErrUnsupported) || !strings.Contains(err.Error(), "/proc/1/net/dev:") {
		t.Errorf("without the mount: %v, want unsupported and how to mount it", err)
	}

	bad := map[string]string{
		"only lo":       "    lo: 1 2 0 0 0 0 0 0 1 2 0 0 0 0 0 0\n",
		"few counters":  "  eth0: 1 2 3\n",
		"not a number":  "  eth0: x 2 0 0 0 0 0 0 1 2 0 0 0 0 0 0\n",
		"empty":         "",
		"headers only":  "Inter-|   Receive |  Transmit\n face |bytes|bytes\n",
		"negative rx":   "  eth0: -1 2 0 0 0 0 0 0 1 2 0 0 0 0 0 0\n",
		"overflowed tx": "  eth0: 1 2 0 0 0 0 0 0 99999999999999999999999 2 0 0 0 0 0 0\n",
	}
	for name, data := range bad {
		if _, err := parseNetDev("dev", []byte(data)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func FuzzParseNetDev(f *testing.F) {
	f.Add([]byte("  eth0: 1 2 0 0 0 0 0 0 1 2 0 0 0 0 0 0\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		parseNetDev("dev", data) // must not panic
	})
}

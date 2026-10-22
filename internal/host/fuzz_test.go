package host

import (
	"math"
	"strings"
	"testing"
)

// FuzzCPULine: no /proc/stat line crashes the parser.
func FuzzCPULine(f *testing.F) {
	f.Add("23830456 25579 16322006 2659329804 3480493 0 577724 0 0 0")
	f.Add("1 2 3")
	f.Fuzz(func(t *testing.T, line string) {
		parseCPUTimes(strings.Fields(line))
	})
}

// FuzzBusy: CPU busy is always a percentage, never NaN or infinite.
func FuzzBusy(f *testing.F) {
	f.Add(uint64(100), uint64(900), uint64(150), uint64(1000))
	f.Fuzz(func(t *testing.T, u1, i1, u2, i2 uint64) {
		if u1 > math.MaxUint64/4 || i1 > math.MaxUint64/4 || u2 > math.MaxUint64/4 || i2 > math.MaxUint64/4 {
			return
		}
		pct, ok := Busy(CPUTimes{User: u1, Idle: i1}, CPUTimes{User: u2, Idle: i2})
		if ok && (math.IsNaN(pct) || pct < 0 || pct > 100) {
			t.Fatalf("Busy = %v", pct)
		}
	})
}

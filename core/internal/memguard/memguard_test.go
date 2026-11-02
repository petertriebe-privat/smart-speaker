package memguard

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// Echo 1 after a reboot, 20.10.2026 (kB).
const echoMeminfo = `MemTotal:         194556 kB
MemFree:            6748 kB
Buffers:            1704 kB
Cached:           157024 kB
Active(anon):      29924 kB
Inactive(anon):     2208 kB
Active(file):      22888 kB
Inactive(file):   120984 kB
Mapped:            38252 kB
Shmem:             14856 kB
SReclaimable:       2120 kB
`

func TestAvailableOnTheEcho(t *testing.T) {
	// 6748 + (22888 + 120984 - 38252) + 2120 = 114488 kB
	if got := Available([]byte(echoMeminfo)); got != 114488<<10 {
		t.Fatalf("available %d kB", got>>10)
	}
	if got := Available([]byte("MemTotal: 1 kB\nMemFree: 10 kB\nMemAvailable: 500 kB\n")); got != 500<<10 {
		t.Fatalf("newer kernels: MemAvailable, got %d kB", got>>10)
	}
	if Available(nil) != 0 {
		t.Fatal("no meminfo: unknown")
	}
}

func statm(mb int64) []byte {
	return []byte(fmt.Sprintf("140000 %d 900 1000 0 30000 0", mb<<20/int64(os.Getpagesize())))
}

func lowMeminfo(freeMB int) []byte {
	return []byte(strings.Replace(strings.Replace(echoMeminfo, "6748", fmt.Sprint(freeMB<<10), 1),
		"120984", "15372", 1)) // file cache - mapped = 0
}

func TestCheck(t *testing.T) {
	l := Limits{Hard: 96 << 20, Soft: 48 << 20, LowAvail: 24 << 20}
	for _, c := range []struct {
		name    string
		rss     int64
		meminfo []byte
		exceed  bool
	}{
		{"normal day (18 MB)", 18, []byte(echoMeminfo), false},
		{"above the hard limit", 120, []byte(echoMeminfo), true},
		{"system low, Jarvis small: someone else", 30, lowMeminfo(10), false},
		{"system low, Jarvis large", 60, lowMeminfo(10), true},
		{"Jarvis large, system fine", 60, []byte(echoMeminfo), false},
		{"no statm (not Linux)", 0, []byte(echoMeminfo), false},
	} {
		s := statm(c.rss)
		if c.rss == 0 {
			s = nil
		}
		if got := check(l, s, c.meminfo); (got != "") != c.exceed {
			t.Errorf("%s: %q", c.name, got)
		}
	}
}

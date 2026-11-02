// Package memguard keeps the assistant from starving the Echo (190 MB RAM, no swap). When a process
// takes too much, the kernel first drops the code pages of everything else; sshd stops answering,
// and the watchdog resets the device (20.10.2026: the mirror read the wake word recordings into
// RAM, Echo 1 hung every two minutes). A working SSH connection comes first, so the assistant ends
// himself before that point and the service loop starts him again.
//
// Three checks, four times a second: The assistant's own resident memory above the hard limit; or the
// system's reclaimable memory low while the assistant holds more than the soft limit. Besides, the Go
// runtime collects harder near the soft limit (debug.SetMemoryLimit), and the assistant offers itself
// to the kernel's OOM killer before anything else (oom_score_adj).
package memguard

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// Limits in bytes.
type Limits struct {
	Hard     int64 // own resident memory that ends the assistant in any case
	Soft     int64 // the Go runtime's memory limit; above it, low system memory ends the assistant
	LowAvail int64 // reclaimable system memory below this is low
}

// OOMScore: The assistant and its children (librespot) go first when the kernel must kill.
const OOMScore = 500

const every = 250 * time.Millisecond

// Start sets the runtime limit and the OOM score and watches; exceeded is called once with the
// reason (it should log and exit).
func Start(l Limits, exceeded func(reason string)) {
	debug.SetMemoryLimit(l.Soft)
	SetOOMScore(os.Getpid(), OOMScore)
	go func() {
		for range time.Tick(every) {
			if reason := check(l, readFile("/proc/self/statm"), readFile("/proc/meminfo")); reason != "" {
				exceeded(reason)
				return
			}
		}
	}()
}

// SetOOMScore: the kernel's preference to kill pid (-1000 never .. 1000 first).
func SetOOMScore(pid, score int) error {
	return os.WriteFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid), []byte(strconv.Itoa(score)), 0o644)
}

func readFile(path string) []byte {
	data, _ := os.ReadFile(path)
	return data
}

// check: "" while all is well, else why the assistant must end.
func check(l Limits, statm, meminfo []byte) string {
	rss := residentBytes(statm)
	if rss <= 0 {
		return "" // not Linux, or unreadable: nothing to judge
	}
	if rss > l.Hard {
		return fmt.Sprintf("The assistant holds %d MB (limit %d MB)", rss>>20, l.Hard>>20)
	}
	if avail := Available(meminfo); avail > 0 && avail < l.LowAvail && rss > l.Soft {
		return fmt.Sprintf("system memory low (%d MB reclaimable) while the assistant holds %d MB", avail>>20, rss>>20)
	}
	return ""
}

// Resident: The assistant's resident size and what the system could still give (bytes; 0 = unknown).
func Resident() (rss, available int64) {
	return residentBytes(readFile("/proc/self/statm")), Available(readFile("/proc/meminfo"))
}

// residentBytes from /proc/self/statm (pages: size resident ...).
func residentBytes(statm []byte) int64 {
	f := strings.Fields(string(statm))
	if len(f) < 2 {
		return 0
	}
	pages, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * int64(os.Getpagesize())
}

// Available: what the kernel can give without killing (bytes) from /proc/meminfo; 0 if unknown.
// The Echo's kernel (2.6.37) has no MemAvailable: free memory plus the file cache that is not
// mapped (mapped pages are running code: dropping them is the thrashing that hangs the device)
// plus reclaimable slab. Shared memory (the DSP's buffers, tmpfs) is on the anon lists, not in
// the file cache.
func Available(meminfo []byte) int64 {
	v := map[string]int64{}
	sc := bufio.NewScanner(bytes.NewReader(meminfo))
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		if n, err := strconv.ParseInt(f[0], 10, 64); err == nil {
			v[name] = n << 10
		}
	}
	if _, ok := v["MemFree"]; !ok {
		return 0
	}
	if a, ok := v["MemAvailable"]; ok {
		return a
	}
	file := v["Active(file)"] + v["Inactive(file)"] - v["Mapped"]
	return v["MemFree"] + max(0, file) + v["SReclaimable"]
}

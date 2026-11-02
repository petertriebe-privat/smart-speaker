package music

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// bindToParent: the kernel kills librespot when the assistant dies (a restart by the service loop would
// otherwise leave the old one running, and Spotify would list the speaker twice).
func bindToParent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

// killStale ends librespot processes of this binary left from an earlier run.
func killStale(bin string) {
	dirs, _ := filepath.Glob("/proc/[0-9]*")
	for _, dir := range dirs {
		cmdline, err := os.ReadFile(dir + "/cmdline")
		if err != nil {
			continue
		}
		if exe, _, _ := strings.Cut(string(cmdline), "\x00"); exe == bin {
			var pid int
			for _, c := range filepath.Base(dir) {
				pid = pid*10 + int(c-'0')
			}
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

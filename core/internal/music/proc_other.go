//go:build !linux

package music

import "os/exec"

func bindToParent(*exec.Cmd) {}
func killStale(string)       {}

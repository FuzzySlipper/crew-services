//go:build !unix

package reviewhook

import "os/exec"

func ownProcessGroup(*exec.Cmd) {}

func killProcessGroup(*exec.Cmd) {}

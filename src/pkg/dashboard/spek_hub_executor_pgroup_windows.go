//go:build windows

package dashboard

import "os/exec"

func spekHubConfigureProcessGroup(*exec.Cmd) {}

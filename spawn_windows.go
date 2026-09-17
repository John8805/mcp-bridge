//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// detachFromConsole gives the child its own console and process group. A
// child that shares the bridge's console can deliver console control events
// (a Ctrl+C raised on shutdown, say) to the bridge itself; a hidden console
// of its own keeps those where they belong.
func detachFromConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | 0x08000000, // CREATE_NO_WINDOW
	}
}

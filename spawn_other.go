//go:build !windows

package main

import "os/exec"

// detachFromConsole is a no-op outside Windows: children are in their own
// session only on request, and console control events do not exist.
func detachFromConsole(_ *exec.Cmd) {}

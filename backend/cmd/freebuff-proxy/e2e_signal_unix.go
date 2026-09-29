//go:build !windows

package main

import (
	"os"
	"os/exec"
)

// prepareChild hooks the subprocess for a graceful shutdown signal. On Unix
// no special setup is needed: os.Interrupt (SIGINT) is delivered directly
// to the child's PID.
func prepareChild(_ *exec.Cmd) {}

// sendShutdownSignal delivers the graceful-shutdown signal (SIGINT) to the
// subprocess. The binary registers shutdownSignals() (os.Interrupt +
// SIGTERM) and drains before exiting 0.
func sendShutdownSignal(cmd *exec.Cmd) error {
	return cmd.Process.Signal(os.Interrupt)
}

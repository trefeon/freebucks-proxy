//go:build windows

package main

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// prepareChild starts the subprocess in its OWN process group so a
// CTRL_BREAK_EVENT can be targeted at exactly it (never this test or the
// surrounding console) — the same CREATE_NEW_PROCESS_GROUP pattern the
// Ctrl+Break drain helper in main_windows_test.go uses.
func prepareChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// sendShutdownSignal delivers CTRL_BREAK_EVENT to the subprocess's process
// group. Go's Windows runtime maps CTRL_BREAK_EVENT to os.Interrupt
// (runtime/os_windows.go ctrlHandler), which the binary's
// shutdownSignals() notify set catches, so the child drains and exits 0.
// The event can only be generated when the process group is attached to a
// console; without one (CI services, detached launchers) it fails and the
// caller skips, mirroring TestCtrlBreakDrainsGracefully's NO_CONSOLE skip.
func sendShutdownSignal(cmd *exec.Cmd) error {
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(cmd.Process.Pid))
}

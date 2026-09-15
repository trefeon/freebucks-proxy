//go:build linux

package server

import (
	"errors"
	"sync/atomic"
	"testing"
)

// TestDoRestartExecFailureFallsBackToExit pins the container re-exec path:
// inside a container the handler reaches execFunc, and when the exec fails
// the process exits via exitProcess instead of silently continuing.
func TestDoRestartExecFailureFallsBackToExit(t *testing.T) {
	oldProbe, oldExe, oldExec, oldExit := containerProbe, executableFunc, execFunc, exitProcess
	defer func() {
		containerProbe, executableFunc, execFunc, exitProcess = oldProbe, oldExe, oldExec, oldExit
	}()

	var exited, execed atomic.Bool
	containerProbe = func() bool { return true }
	executableFunc = func() (string, error) { return "/proc/self/exe", nil }
	execFunc = func(path string, argv []string, env []string) error {
		execed.Store(true)
		return errors.New("boom")
	}
	exitProcess = func() { exited.Store(true) }

	doRestart()

	if !execed.Load() {
		t.Errorf("execFunc was not called inside a container")
	}
	if !exited.Load() {
		t.Errorf("exitProcess was not called after exec failure")
	}
}

// TestDoRestartExecutableErrorFallsBackToExit pins the resolve-error path:
// an unresolvable binary still exits (today's behavior) instead of hanging.
func TestDoRestartExecutableErrorFallsBackToExit(t *testing.T) {
	oldProbe, oldExe, oldExec, oldExit := containerProbe, executableFunc, execFunc, exitProcess
	defer func() {
		containerProbe, executableFunc, execFunc, exitProcess = oldProbe, oldExe, oldExec, oldExit
	}()

	var exited, execed atomic.Bool
	containerProbe = func() bool { return true }
	executableFunc = func() (string, error) { return "", errors.New("no exe") }
	execFunc = func(path string, argv []string, env []string) error {
		execed.Store(true)
		return nil
	}
	exitProcess = func() { exited.Store(true) }

	doRestart()

	if execed.Load() {
		t.Errorf("execFunc was called despite executable resolution failure")
	}
	if !exited.Load() {
		t.Errorf("exitProcess was not called after executable resolution failure")
	}
}

// TestDoRestartOutsideContainerExits pins the non-container path: no exec,
// straight to exit (today's service-manager behavior, unchanged).
func TestDoRestartOutsideContainerExits(t *testing.T) {
	oldProbe, oldExec, oldExit := containerProbe, execFunc, exitProcess
	defer func() {
		containerProbe, execFunc, exitProcess = oldProbe, oldExec, oldExit
	}()

	var exited, execed atomic.Bool
	containerProbe = func() bool { return false }
	execFunc = func(path string, argv []string, env []string) error {
		execed.Store(true)
		return nil
	}
	exitProcess = func() { exited.Store(true) }

	doRestart()

	if execed.Load() {
		t.Errorf("execFunc was called outside a container")
	}
	if !exited.Load() {
		t.Errorf("exitProcess was not called outside a container")
	}
}

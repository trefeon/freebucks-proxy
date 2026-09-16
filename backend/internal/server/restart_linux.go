//go:build linux

package server

import (
	"log/slog"
	"os"
	"syscall"
)

// executableFunc resolves the current binary for re-exec. A var so tests
// can force the resolve-error fallback.
var executableFunc = os.Executable

// execFunc replaces the process image. A var so tests can force the
// exec-error fallback without replacing the test binary.
var execFunc = syscall.Exec

// doRestart re-execs the gateway image in place (same PID, same container)
// when running in a container; outside a container it exits so the service
// manager restarts it as before. Any failure falls back to exit.
func doRestart() {
	if !containerProbe() {
		exitProcess()
		return
	}
	exe, err := executableFunc()
	if err != nil {
		slog.Default().Error("restart: cannot resolve executable, exiting", "err", err)
		exitProcess()
		return
	}
	if err := execFunc(exe, os.Args, os.Environ()); err != nil {
		slog.Default().Error("restart: re-exec failed, exiting", "err", err)
		exitProcess()
	}
}

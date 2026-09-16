//go:build !linux

package server

// doRestart on non-linux platforms keeps today's behavior: exit and let the
// service manager restart the process. syscall.Exec is linux-only, so this
// file keeps Windows/macOS builds compiling.
func doRestart() {
	exitProcess()
}

package server

import (
	"os"
	"strings"
)

// restartProcess re-execs the gateway when running in a container and
// exits otherwise. It runs 200ms after the 200 response flushes
// (handleAdminRestart), so neither path drains in-flight requests today:
// cut connections rely on SESSION_PERSIST to resume.
var restartProcess = func() {
	doRestart()
}

// exitProcess ends the process (today's restart behavior outside a
// container, and the fallback when re-exec fails). A var so tests can
// observe the fallback without exiting.
var exitProcess = func() {
	os.Exit(0)
}

// containerProbe reports whether the process runs inside a container. A
// var so tests can force either path without touching the host fs.
var containerProbe = isInContainer

// dockerenvPath and cgroupPath are the host markers isInContainer reads.
const (
	dockerenvPath = "/.dockerenv"
	cgroupPath    = "/proc/1/cgroup"
)

// isInContainer reports whether the process runs inside a container,
// checked against the live host paths.
func isInContainer() bool {
	return inContainer(dockerenvPath, cgroupPath)
}

// inContainer reports container membership against injected paths so tests
// can pin each signal: /.dockerenv present, or the cgroup file mentioning
// docker, kubepods (kubernetes), or containerd. Missing/unreadable files
// count as not-container (bare-metal / plain VM boot).
func inContainer(dockerenv, cgroup string) bool {
	if _, err := os.Stat(dockerenv); err == nil {
		return true
	}
	data, err := os.ReadFile(cgroup)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "docker") ||
			strings.Contains(lower, "kubepods") ||
			strings.Contains(lower, "containerd") {
			return true
		}
	}
	return false
}

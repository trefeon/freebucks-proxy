// The static client-environment descriptor (vendor cli/src/utils/
// client-environment.ts + common/src/constants/freebuff-client-descriptor.ts):
// a compact summary of the environment, sent as x-freebuff-env on session
// and ad requests.
//
// Only presence flags, a size, and fixed bucket names — never a raw
// environment value, path, or process name. The CLI fills several buckets
// from its live terminal (TTY flags, size, terminal program, ancestor
// processes, colour-query reply, TLS overrides); the proxy is a headless
// server and reports itself honestly as one: no TTY, no terminal program,
// unknown ancestry, and only the buckets a gateway can answer without
// inventing a terminal it does not have (CI presence, proxy-egress bucket).
//
// Shape (vendor formatClientEnvironment field order):
// v1;in=0;out=0;tp=none;term=0;ct=0;sz=0x0;ci=0;ssh=0;l=0;p=unknown;
// g=unknown;osc=na;tzo=0;px=none;tls=1;ca=0
package upstream

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
)

const (
	// FreebuffEnvHeader carries the client-environment descriptor (vendor
	// FREEBUFF_CLIENT_ENV_HEADER), stamped on session and ad requests.
	FreebuffEnvHeader = "x-freebuff-env"
)

var (
	clientEnvMu    sync.Mutex
	clientEnvBuilt bool
	clientEnvText  string
)

// resetClientEnvForTest drops the cached descriptor so tests can rebuild it
// under controlled environment variables (vendor
// resetClientEnvironmentForTest). Tests only.
func resetClientEnvForTest() {
	clientEnvMu.Lock()
	defer clientEnvMu.Unlock()
	clientEnvBuilt = false
	clientEnvText = ""
}

// clientEnvDescriptor returns the process's static environment descriptor,
// built once (the CLI likewise builds it once per process and only fills in
// the async parts as they resolve — a server has no async parts).
func clientEnvDescriptor() string {
	clientEnvMu.Lock()
	defer clientEnvMu.Unlock()
	if !clientEnvBuilt {
		clientEnvText = "v1" +
			";in=0" + // no stdin TTY on a server process
			";out=0" + // no stdout TTY on a server process
			";tp=none" +
			";term=0" +
			";ct=0" +
			";sz=0x0" +
			";ci=" + clientEnvCIFlag() +
			";ssh=0" +
			";l=0" +
			";p=unknown" + // no ancestor-process lookup on a server
			";g=unknown" +
			";osc=na" +
			";tzo=0" + // no TZ override: the server declares its zone elsewhere
			";px=" + proxyEgressBucket() +
			";tls=1" +
			";ca=0"
		clientEnvBuilt = true
	}
	return clientEnvText
}

// clientEnvCIFlag mirrors the vendor CI detection (CI=true|1 or
// GITHUB_ACTIONS=true).
func clientEnvCIFlag() string {
	if os.Getenv("CI") == "true" || os.Getenv("CI") == "1" || os.Getenv("GITHUB_ACTIONS") == "true" {
		return "1"
	}
	return "0"
}

// proxyEgressBucket reduces the proxy environment to where it points
// (vendor bucketProxy: loopback = a proxy on this machine). Only the bucket
// is ever reported, never the URL.
func proxyEgressBucket() string {
	for _, name := range []string{
		"HTTPS_PROXY", "https_proxy",
		"HTTP_PROXY", "http_proxy",
		"ALL_PROXY", "all_proxy",
	} {
		if raw := strings.TrimSpace(os.Getenv(name)); raw != "" {
			return bucketProxyURL(raw)
		}
	}
	return "none"
}

// bucketProxyURL maps one proxy URL to its bucket: loopback when the host is
// this machine, remote otherwise. An unset-bucket input never reaches here
// (empty values are skipped above); an unparseable value still names a
// configured proxy, so it buckets remote rather than claiming none.
func bucketProxyURL(raw string) string {
	host := ""
	if u, err := url.Parse(raw); err == nil {
		host = strings.ToLower(u.Hostname())
	}
	if host == "" && !strings.Contains(raw, "://") {
		// "host:port" without a scheme still names a proxy: reparse with
		// a scheme so "[::1]:8080" judges its bracketed host, not its
		// first colon token.
		if u, err := url.Parse("http://" + raw); err == nil {
			host = strings.ToLower(u.Hostname())
		}
	}
	if host == "localhost" || host == "::1" || host == "0.0.0.0" || strings.HasPrefix(host, "127.") {
		return "loopback"
	}
	return "remote"
}

// stampClientEnv sets the environment descriptor header (vendor
// clientEnvironmentHeaders, spread into every session and ad request).
func stampClientEnv(header http.Header) {
	header.Set(FreebuffEnvHeader, clientEnvDescriptor())
}

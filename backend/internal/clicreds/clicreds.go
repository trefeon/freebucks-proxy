// Package clicreds reads FreeBuff credentials from the official CLI login
// files. It is a small, standalone helper so the bottom-layer config package
// can accept a discovery function without importing product-specific file
// formats (issue #283).
package clicreds

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ConfigDir resolves the official CLI config directory, mirroring upstream
// cli/src/utils/config-dir.ts exactly:
//
//  1. $FREEBUFF_CONFIG_DIR when set: an absolute path is used verbatim; a
//     relative value is rejected (error) so CLI settings can never be read
//     or written relative to the current project.
//  2. Otherwise ~/.config/manicode, suffixed with
//     "-$NEXT_PUBLIC_CB_ENVIRONMENT" when that variable is set and != "prod"
//     (development-stack dirs such as ~/.config/manicode-staging).
func ConfigDir() (string, error) {
	if override := os.Getenv("FREEBUFF_CONFIG_DIR"); override != "" {
		if !filepath.IsAbs(override) {
			return "", fmt.Errorf("FREEBUFF_CONFIG_DIR must be an absolute path, got %q", override)
		}
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("cannot resolve home directory")
	}
	name := "manicode"
	if env := os.Getenv("NEXT_PUBLIC_CB_ENVIRONMENT"); env != "" && env != "prod" {
		name += "-" + env
	}
	return filepath.Join(home, ".config", name), nil
}

// DiscoverToken auto-discovers the CLI auth token the same way the official
// FreeBuff CLI resolves it (upstream cli/src/utils/auth.ts: userFromJson
// defaults to profile "default", getAuthTokenDetails reads field "authToken"
// only):
//
//   - profile "default" only (a "freebuff"/"codebuff"/any other profile is
//     ignored, whether "default" is present or not);
//   - field "authToken" only (no "sessionToken"/"token" fallbacks).
//
// The credentials file is <ConfigDir()>/credentials.json. The hardcoded
// ~/.config/freebuff and ~/.config/codebuff locations survive ONLY as
// last-resort fallbacks when neither the $FREEBUFF_CONFIG_DIR override nor
// the $NEXT_PUBLIC_CB_ENVIRONMENT suffix is in play (legacy installs
// predating the single manicode dir).
func DiscoverToken() (token, email, path string, ok bool) {
	dir, err := ConfigDir()
	if err != nil {
		return "", "", "", false
	}
	if token, email, path, ok := tokenFromFile(filepath.Join(dir, "credentials.json")); ok {
		return token, email, path, true
	}
	for _, fallback := range legacyFallbackFiles() {
		if token, email, path, ok := tokenFromFile(fallback); ok {
			return token, email, path, true
		}
	}
	return "", "", "", false
}

// legacyFallbackFiles returns the pre-manicode hardcoded credential paths.
// Non-empty only when ConfigDir fell back to the plain default — i.e. no
// $FREEBUFF_CONFIG_DIR override and no $NEXT_PUBLIC_CB_ENVIRONMENT suffix —
// so an explicit dir selection is never widened back into a multi-dir scan.
func legacyFallbackFiles() []string {
	if os.Getenv("FREEBUFF_CONFIG_DIR") != "" {
		return nil
	}
	if env := os.Getenv("NEXT_PUBLIC_CB_ENVIRONMENT"); env != "" && env != "prod" {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	return []string{
		filepath.Join(home, ".config", "freebuff", "credentials.json"),
		filepath.Join(home, ".config", "codebuff", "credentials.json"),
	}
}

func tokenFromFile(path string) (token, email string, file string, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", "", false
	}
	// Strip a leading UTF-8 BOM (Windows credential writers can add one)
	// or json.Unmarshal fails and auto-discovery silently skips the file.
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", "", "", false
	}
	acct, _ := parsed["default"].(map[string]any)
	if acct == nil {
		return "", "", "", false
	}
	rawToken, _ := acct["authToken"].(string)
	if strings.TrimSpace(rawToken) == "" {
		return "", "", "", false
	}
	rawEmail, _ := acct["email"].(string)
	return strings.TrimSpace(rawToken), strings.TrimSpace(rawEmail), path, true
}

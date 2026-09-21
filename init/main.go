package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type MMDSData struct {
	Token      string            `json:"token"`
	Env        map[string]string `json:"env"`
	Entrypoint string            `json:"entrypoint"`
	Port       int               `json:"port"`
	DNS        []string          `json:"dns"`
}

// Bounded bootstrap retry policy. The worker waits up to 30s for guest
// readiness, so we keep the worst case well under that:
// 3 attempts * 5s HTTP timeout + 0.5s + 1s backoff = 16.5s.
const (
	metadataFetchAttempts = 3
	metadataFetchBackoff  = 500 * time.Millisecond
)

func main() {
	bootToken := parseBootToken()

	var argv []string
	if len(os.Args) > 1 {
		argv = os.Args[1:]
	}

	// Only fetch bootstrap metadata when the kernel command line told us a
	// token is expected. This is the "bootstrap was expected" signal.
	var (
		metadata *MMDSData
		fetchErr error
	)
	if bootToken != "" {
		metadata, fetchErr = fetchMetadataWithRetry(bootToken, metadataFetchAttempts, metadataFetchBackoff)
	}

	args, env, err := resolveBoot(bootToken, argv, metadata, fetchErr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aether-env: refusing to start: %v\n", err)
		os.Exit(1)
	}

	// Set env vars.
	for key, value := range env {
		os.Setenv(key, value)
	}

	// DNS is optional. An empty list preserves the previous behavior of
	// leaving /etc/resolv.conf untouched.
	if metadata != nil && len(metadata.DNS) > 0 {
		if err := writeResolvConf("/etc/resolv.conf", metadata.DNS); err != nil {
			fmt.Fprintf(os.Stderr, "aether-env: warning: failed to write /etc/resolv.conf: %v\n", err)
		}
	}

	binary, err := exec.LookPath(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "aether-env: command not found: %s\n", args[0])
		os.Exit(127)
	}

	fmt.Fprintf(os.Stderr, "aether-env: running %v\n", args)
	if err := syscall.Exec(binary, args, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "aether-env: exec failed: %v\n", err)
		os.Exit(1)
	}
}

// resolveBoot is the pure fail-closed decision function. It selects the
// command to exec and the environment to apply, given:
//   - bootToken: the token parsed from the kernel command line ("" if absent)
//   - argv: the explicit command from os.Args[1:] (nil if absent)
//   - metadata: the MMDS payload (nil if it was not fetched)
//   - fetchErr: the error from fetching/validating the MMDS payload
//
// Policy:
//   - token present and metadata could not be loaded/verified -> error
//     (never fall back to handler.js)
//   - token absent and argv present -> legacy explicit-command path
//   - token absent and no argv -> error (nothing legitimate to run)
func resolveBoot(bootToken string, argv []string, metadata *MMDSData, fetchErr error) ([]string, map[string]string, error) {
	hasArgv := len(argv) > 0

	if bootToken != "" {
		if fetchErr != nil {
			return nil, nil, fmt.Errorf("bootstrap metadata was expected (boot token present) but could not be loaded: %w", fetchErr)
		}
		if metadata == nil {
			return nil, nil, errors.New("bootstrap metadata was expected (boot token present) but none was returned")
		}
		if metadata.Token != bootToken {
			return nil, nil, fmt.Errorf("bootstrap token mismatch (expected %q, got %q)", bootToken, metadata.Token)
		}
		if hasArgv {
			return argv, metadata.Env, nil
		}
		entrypoint := metadata.Entrypoint
		if entrypoint == "" {
			entrypoint = "handler.js"
		}
		return getCommandForEntrypoint(entrypoint), metadata.Env, nil
	}

	if hasArgv {
		// Legacy explicit-command path, e.g. aether-env node handler.js.
		return argv, nil, nil
	}

	return nil, nil, errors.New("no boot token and no command provided; nothing to run")
}

func getCommandForEntrypoint(entrypoint string) []string {
	switch {
	case strings.HasSuffix(entrypoint, ".js"):
		return []string{"node", entrypoint}
	case strings.HasSuffix(entrypoint, ".py"):
		return []string{"python3", entrypoint}
	case strings.HasSuffix(entrypoint, ".sh"):
		return []string{"sh", entrypoint}
	default:
		// Assume it's an executable
		return []string{"./" + entrypoint}
	}
}

func parseBootToken() string {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return ""
	}

	for _, arg := range strings.Fields(string(data)) {
		if strings.HasPrefix(arg, "aether_token=") {
			return strings.TrimPrefix(arg, "aether_token=")
		}
	}
	return ""
}

// fetchMetadataWithRetry calls fetchMetadata up to attempts times, sleeping
// with an exponential backoff between tries. It returns the last error if all
// attempts fail.
func fetchMetadataWithRetry(bootToken string, attempts int, baseBackoff time.Duration) (*MMDSData, error) {
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(baseBackoff * time.Duration(1<<(i-1)))
		}
		metadata, err := fetchMetadata(bootToken)
		if err == nil {
			return metadata, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// renderResolvConf renders the contents of a resolv.conf from a list of
// nameserver addresses. It returns nil when there is nothing to write, which
// signals to the caller that /etc/resolv.conf must be left untouched.
func renderResolvConf(dns []string) []byte {
	var b strings.Builder
	for _, ip := range dns {
		ip = strings.TrimSpace(ip)
		if ip == "" {
			continue
		}
		fmt.Fprintf(&b, "nameserver %s\n", ip)
	}
	if b.Len() == 0 {
		return nil
	}
	return []byte(b.String())
}

// writeResolvConf writes /etc/resolv.conf from the given DNS list. An empty or
// whitespace-only list is a no-op.
func writeResolvConf(path string, dns []string) error {
	content := renderResolvConf(dns)
	if content == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("failed to create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	return nil
}

func fetchMetadata(bootToken string) (*MMDSData, error) {
	client := &http.Client{Timeout: 5 * time.Second}

	req, err := http.NewRequest("GET", "http://169.254.169.254/", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch MMDS: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("MMDS returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read MMDS response: %w", err)
	}

	var metadata MMDSData
	if err := json.Unmarshal(body, &metadata); err != nil {
		return nil, fmt.Errorf("invalid JSON from MMDS (got: %.100s): %w", string(body), err)
	}

	if metadata.Token != bootToken {
		return nil, fmt.Errorf("token mismatch (expected %s, got %s)", bootToken, metadata.Token)
	}

	return &metadata, nil
}

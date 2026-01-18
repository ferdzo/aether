package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

type MMDSData struct {
	Token      string            `json:"token"`
	Env        map[string]string `json:"env"`
	Entrypoint string            `json:"entrypoint"`
	Port       int               `json:"port"`
}

func main() {
	bootToken := parseBootToken()

	var metadata *MMDSData
	if bootToken != "" {
		var err error
		metadata, err = fetchMetadata(bootToken)
		if err != nil {
			fmt.Fprintf(os.Stderr, "aether-env: warning: %v (continuing with defaults)\n", err)
			metadata = &MMDSData{Entrypoint: "handler.js"}
		}
	} else {
		fmt.Fprintln(os.Stderr, "aether-env: no boot token, using defaults")
		metadata = &MMDSData{Entrypoint: "handler.js"}
	}

	// Set env vars
	for key, value := range metadata.Env {
		os.Setenv(key, value)
	}

	// Determine command to run
	var args []string
	if len(os.Args) > 1 {
		// If args provided, use them (e.g., aether-env node handler.js)
		args = os.Args[1:]
	} else {
		// Auto-detect runtime from entrypoint
		entrypoint := metadata.Entrypoint
		if entrypoint == "" {
			entrypoint = "handler.js"
		}
		args = getCommandForEntrypoint(entrypoint)
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


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
	Token string            `json:"token"`
	Env   map[string]string `json:"env"`
}

func main() {
	bootToken := parseBootToken()
	
	var envVars map[string]string
	if bootToken != "" {
		var err error
		envVars, err = fetchEnvVars(bootToken)
		if err != nil {
			fmt.Fprintf(os.Stderr, "aether-env: warning: %v (continuing without env vars)\n", err)
		}
	} else {
		fmt.Fprintln(os.Stderr, "aether-env: no boot token, skipping MMDS")
	}

	if len(os.Args) == 1 {
		for key, value := range envVars {
			escaped := strings.ReplaceAll(value, "'", "'\"'\"'")
			fmt.Printf("export %s='%s'\n", key, escaped)
		}
		return
	}

	// Set env vars
	for key, value := range envVars {
		os.Setenv(key, value)
	}

	// Exec the command
	binary, err := exec.LookPath(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "aether-env: command not found: %s\n", os.Args[1])
		os.Exit(127)
	}

	if err := syscall.Exec(binary, os.Args[1:], os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "aether-env: exec failed: %v\n", err)
		os.Exit(1)
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

func fetchEnvVars(bootToken string) (map[string]string, error) {
	client := &http.Client{Timeout: 5 * time.Second}

	// Request JSON format from MMDS
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

	return metadata.Env, nil
}

